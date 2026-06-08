package openai

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var functionNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)

type unsupportedField struct {
	name    string
	message string
	allow   func(any) bool
}

var alwaysRejectChatFields = []unsupportedField{
	{name: "audio", message: "audio output is not supported"},
	{name: "function_call", message: "legacy function_call is not supported; use tools"},
	{name: "functions", message: "legacy functions are not supported; use tools"},
	{name: "logit_bias", message: "logit_bias is not supported"},
	{name: "logprobs", message: "logprobs is not supported"},
	{name: "top_logprobs", message: "top_logprobs is not supported"},
	{name: "max_tokens", message: "max_tokens is not supported by this proxy in MVP"},
	{name: "max_completion_tokens", message: "max_completion_tokens is not supported by this proxy in MVP"},
	{name: "modalities", message: "modalities are not supported"},
	{name: "prediction", message: "prediction is not supported"},
	{name: "response_format", message: "response_format is not supported in MVP"},
	{name: "stop", message: "stop sequences are not supported by this backend"},
	{name: "n", message: "n other than 1 is not supported", allow: isOne},
}

var strictOnlyChatFields = []unsupportedField{
	{name: "temperature", message: "temperature is not forwarded by this proxy in MVP"},
	{name: "top_p", message: "top_p is not forwarded by this proxy in MVP"},
	{name: "presence_penalty", message: "presence_penalty is not forwarded by this proxy in MVP"},
	{name: "frequency_penalty", message: "frequency_penalty is not forwarded by this proxy in MVP"},
	{name: "seed", message: "seed is not supported"},
	{name: "metadata", message: "metadata is not supported on chat completions"},
	{name: "service_tier", message: "service_tier is not supported"},
	{name: "user", message: "user is not forwarded by this single-user proxy"},
}

var alwaysRejectResponseFields = []unsupportedField{
	{name: "background", message: "background mode is not supported"},
	{name: "max_output_tokens", message: "max_output_tokens is not supported by this proxy in MVP"},
	{name: "truncation", message: "truncation controls are not supported in MVP"},
}

var strictOnlyResponseFields = []unsupportedField{
	{name: "temperature", message: "temperature is not forwarded by this proxy in MVP"},
	{name: "top_p", message: "top_p is not forwarded by this proxy in MVP"},
	{name: "include", message: "include is ignored by this proxy in permissive mode"},
	{name: "reasoning", message: "reasoning object controls are only partially supported in permissive mode; use reasoning_effort"},
	{name: "text", message: "text controls are ignored by this proxy in permissive mode"},
	{name: "metadata", message: "metadata is not supported in MVP"},
	{name: "service_tier", message: "service_tier is not supported"},
	{name: "user", message: "user is not forwarded by this single-user proxy"},
}

func ValidateChatRequest(req *ChatCompletionRequest, strict bool) error {
	if req.Model == "" {
		return InvalidRequest("model is required", "model")
	}
	if len(req.Messages) == 0 {
		return InvalidRequest("messages is required", "messages")
	}
	if err := validateUnsupportedFields(req.Raw, alwaysRejectChatFields); err != nil {
		return err
	}
	if strict {
		if err := validateUnsupportedFields(req.Raw, strictOnlyChatFields); err != nil {
			return err
		}
	}
	// parallel_tool_calls=true is accepted: the internal toolproxy batch model
	// already captures and replays multiple tool calls per turn, so there is no
	// reason to reject the OpenAI default.
	if err := ValidateTools(req.Tools); err != nil {
		return err
	}
	if err := validateToolChoice(req.ToolChoice); err != nil {
		return err
	}
	for i, msg := range req.Messages {
		switch msg.Role {
		case "system", "developer", "user", "assistant", "tool":
		default:
			return InvalidRequest(fmt.Sprintf("unsupported message role %q", msg.Role), fmt.Sprintf("messages.%d.role", i))
		}
		if msg.Role == "tool" && msg.ToolCallID == "" {
			return InvalidRequest("tool messages require tool_call_id", fmt.Sprintf("messages.%d.tool_call_id", i))
		}
		if msg.Role != "assistant" && len(msg.ToolCalls) > 0 {
			return InvalidRequest("tool_calls are only valid on assistant messages", fmt.Sprintf("messages.%d.tool_calls", i))
		}
		var err error
		if msg.Role == "user" {
			_, err = msg.Prompt()
		} else {
			_, err = msg.Text()
		}
		if err != nil {
			return InvalidRequest(err.Error(), fmt.Sprintf("messages.%d.content", i))
		}
	}
	return nil
}

func ValidateResponsesRequest(req *ResponsesRequest, strict bool) error {
	if req.Model == "" {
		return InvalidRequest("model is required", "model")
	}
	if len(req.Input) == 0 || string(req.Input) == "null" {
		if req.PreviousResponseID == "" {
			return InvalidRequest("input is required", "input")
		}
	}
	if err := validateUnsupportedFields(req.Raw, alwaysRejectResponseFields); err != nil {
		return err
	}
	if strict {
		if err := validateUnsupportedFields(req.Raw, strictOnlyResponseFields); err != nil {
			return err
		}
	}
	if err := validateResponsesInclude(req.Include); err != nil {
		return err
	}
	if err := validateResponsesReasoning(req); err != nil {
		return err
	}
	if err := validateResponsesText(req.Text); err != nil {
		return err
	}
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		return InvalidRequest("parallel_tool_calls=false is not supported for Responses through this backend", "parallel_tool_calls")
	}
	if err := ValidateResponsesTools(req.Tools, strict); err != nil {
		return err
	}
	if err := validateToolChoice(req.ToolChoice); err != nil {
		return err
	}
	return nil
}

func validateResponsesInclude(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return InvalidRequest("include must be an array of strings", "include")
	}
	for _, value := range values {
		if value != "reasoning.encrypted_content" {
			return InvalidRequest("unsupported include value", "include")
		}
	}
	return nil
}

func validateResponsesReasoning(req *ResponsesRequest) error {
	if len(req.Reasoning) == 0 || string(req.Reasoning) == "null" {
		return nil
	}
	fields, err := rawObject(req.Reasoning, "reasoning")
	if err != nil {
		return err
	}
	for name := range fields {
		switch name {
		case "effort", "summary":
		default:
			return InvalidRequest("unsupported reasoning field", "reasoning."+name)
		}
	}
	if raw, ok := fields["effort"]; ok && string(raw) != "null" {
		var effort string
		if err := json.Unmarshal(raw, &effort); err != nil || effort == "" {
			return InvalidRequest("reasoning.effort must be a string", "reasoning.effort")
		}
		if req.ReasoningEffort != "" && req.ReasoningEffort != effort {
			return InvalidRequest("reasoning.effort conflicts with reasoning_effort", "reasoning.effort")
		}
	}
	if raw, ok := fields["summary"]; ok && string(raw) != "null" {
		var summary string
		if err := json.Unmarshal(raw, &summary); err != nil {
			return InvalidRequest("reasoning.summary must be a string", "reasoning.summary")
		}
	}
	return nil
}

func validateResponsesText(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	fields, err := rawObject(raw, "text")
	if err != nil {
		return err
	}
	for name := range fields {
		switch name {
		case "verbosity", "format":
		default:
			return InvalidRequest("unsupported text field", "text."+name)
		}
	}
	if raw, ok := fields["verbosity"]; ok && string(raw) != "null" {
		var verbosity string
		if err := json.Unmarshal(raw, &verbosity); err != nil || verbosity == "" {
			return InvalidRequest("text.verbosity must be a string", "text.verbosity")
		}
	}
	if raw, ok := fields["format"]; ok && string(raw) != "null" {
		return InvalidRequest("text.format is not supported in MVP", "text.format")
	}
	return nil
}

func rawObject(raw json.RawMessage, param string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, InvalidRequest(param+" must be an object", param)
	}
	return fields, nil
}

func ResponsesReasoningEffort(req *ResponsesRequest) string {
	if req.ReasoningEffort != "" {
		return req.ReasoningEffort
	}
	if len(req.Reasoning) == 0 || string(req.Reasoning) == "null" {
		return ""
	}
	fields, err := rawObject(req.Reasoning, "reasoning")
	if err != nil {
		return ""
	}
	raw, ok := fields["effort"]
	if !ok || string(raw) == "null" {
		return ""
	}
	var effort string
	if err := json.Unmarshal(raw, &effort); err != nil {
		return ""
	}
	return effort
}

func validateUnsupportedFields(raw map[string]any, fields []unsupportedField) error {
	for _, field := range fields {
		value, ok := raw[field.name]
		if !ok {
			continue
		}
		if field.allow != nil && field.allow(value) {
			continue
		}
		return InvalidRequest(field.message, field.name)
	}
	return nil
}

func ValidateTools(tools []Tool) error {
	return validateTools(tools, false)
}

func ValidateResponsesTools(tools []Tool, strict bool) error {
	return validateTools(tools, !strict)
}

func validateTools(tools []Tool, allowUnsupported bool) error {
	seen := map[string]struct{}{}
	for i, tool := range tools {
		if tool.Type != "function" {
			if allowUnsupported {
				continue
			}
			return InvalidRequest("only function tools are supported", fmt.Sprintf("tools.%d.type", i))
		}
		fn := tool.Function
		if !functionNameRE.MatchString(fn.Name) {
			return InvalidRequest("function tool name must match ^[A-Za-z_][A-Za-z0-9_-]{0,63}$", fmt.Sprintf("tools.%d.function.name", i))
		}
		if _, ok := seen[fn.Name]; ok {
			return InvalidRequest("duplicate function tool name", fmt.Sprintf("tools.%d.function.name", i))
		}
		seen[fn.Name] = struct{}{}
		if len(fn.Parameters) > 0 {
			var js any
			if err := json.Unmarshal(fn.Parameters, &js); err != nil {
				return InvalidRequest("function parameters must be valid JSON Schema", fmt.Sprintf("tools.%d.function.parameters", i))
			}
		}
	}
	return nil
}

func SupportedTools(tools []Tool) []Tool {
	out := make([]Tool, 0, len(tools))
	for _, tool := range tools {
		if tool.Type == "function" {
			out = append(out, tool)
		}
	}
	return out
}

func validateToolChoice(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none":
			return nil
		case "required":
			return InvalidRequest("tool_choice=required is not supported by this backend", "tool_choice")
		default:
			return InvalidRequest("unsupported tool_choice", "tool_choice")
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return InvalidRequest("tool_choice must be auto, none, required, or a function object", "tool_choice")
	}
	if obj.Type == "function" {
		return InvalidRequest("forced function tool_choice is not supported by this backend", "tool_choice")
	}
	return InvalidRequest("unsupported tool_choice", "tool_choice")
}

func ToolChoiceNone(raw json.RawMessage) bool {
	var s string
	return len(raw) > 0 && json.Unmarshal(raw, &s) == nil && s == "none"
}

func isOne(v any) bool {
	switch x := v.(type) {
	case json.Number:
		return x.String() == "1" || x.String() == "1.0"
	case float64:
		return x == 1
	case int:
		return x == 1
	default:
		return false
	}
}

func FoldChatInstructions(messages []ChatMessage) (string, []ChatMessage, error) {
	var parts []string
	idx := 0
	for idx < len(messages) {
		role := messages[idx].Role
		if role != "system" && role != "developer" {
			break
		}
		text, err := messages[idx].Text()
		if err != nil {
			return "", nil, err
		}
		if strings.TrimSpace(text) != "" {
			label := "System"
			if role == "developer" {
				label = "Developer"
			}
			parts = append(parts, label+":\n"+text)
		}
		idx++
	}
	for i := idx; i < len(messages); i++ {
		if messages[i].Role == "system" || messages[i].Role == "developer" {
			return "", nil, InvalidRequest("system/developer messages are only supported at the start of the conversation", fmt.Sprintf("messages.%d.role", i))
		}
	}
	return strings.Join(parts, "\n\n"), messages[idx:], nil
}

func InstructionCandidates(s string) []string {
	if s != "" {
		return []string{s}
	}
	return []string{" ", "You are a chat completion model."}
}

func EffectiveInstructions(s string) string {
	if s != "" {
		return s
	}
	return "You are a chat completion model."
}
