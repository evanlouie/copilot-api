package openai

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/evanlouie/copilot-api/internal/apierr"
)

// Request validation follows a single policy on both surfaces:
//
//	Reject unknown. Accept-and-ignore known-but-unsupported, but only when
//	ignoring degrades gracefully. Never silently drop something the client could
//	have meant.
//
// A parameter OpenAI documents but the Copilot SDK cannot honour (max_tokens,
// parallel_tool_calls=false, a forcing tool_choice) is accepted here and
// reported at debug level by the HTTP layer: ignoring it yields a longer answer,
// or an answer that skips a tool call, which is degraded but still usable prose.
// A parameter whose whole purpose is the *shape* of the reply is different.
// Ignoring response_format/text.format json_schema or json_object hands free
// prose to a client that is about to call JSON.parse on it, so the failure lands
// far from its cause or, worse, parses into silently wrong data; those are
// rejected. {"type":"text"} is the documented default, so ignoring it is a
// genuine no-op and it is accepted. A parameter, value, or tool type OpenAI does
// not document is a 400 so client typos surface instead of being dropped.
//
// There is one policy, not two. What this proxy cannot honour but can ignore
// gracefully is reported to the operator through the debug-level logging in
// internal/httpapi rather than through the client's status code, because a 400
// is the client's problem and an unhonoured temperature is the operator's.

// functionNameRE mirrors OpenAI's documented function-name grammar ("a-z, A-Z,
// 0-9, underscores and dashes, with a maximum length of 64"), which allows a
// leading digit such as 2fa_verify. It is deliberately not the same constraint
// as ResponsesSDKToolNameRE: that one guards the identifiers handed to the
// Copilot SDK, and names failing it are aliased by SafeResponsesSDKAlias rather
// than rejected.
var functionNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type unsupportedField struct {
	name    string
	message string
	allow   func(json.RawMessage) bool
}

var alwaysRejectChatFields = []unsupportedField{
	{name: "audio", message: "audio output is not supported"},
	{name: "function_call", message: "legacy function_call is not supported; use tools"},
	{name: "functions", message: "legacy functions are not supported; use tools"},
	{name: "logit_bias", message: "logit_bias is not supported"},
	{name: "modalities", message: "modalities are not supported"},
	{name: "prediction", message: "prediction is not supported"},
	{name: "response_format", message: structuredOutputMessage("response_format"), allow: isDefaultOutputFormat},
	{name: "stop", message: "stop sequences are not supported by this backend"},
	{name: "n", message: "n other than 1 is not supported", allow: isOne},
}

var alwaysRejectResponseFields = []unsupportedField{
	{name: "background", message: "background mode is not supported"},
	{name: "truncation", message: "truncation controls are not supported in MVP"},
}

func ValidateChatRequest(req *ChatCompletionRequest) error {
	if req.Model == "" {
		return apierr.InvalidRequest("model is required", "model")
	}
	if len(req.Messages) == 0 {
		return apierr.InvalidRequest("messages is required", "messages")
	}
	if err := ValidateReasoningEffort(req.ReasoningEffort, "reasoning_effort"); err != nil {
		return err
	}
	// The shape of response_format decides which error the client gets: a
	// malformed object or an undocumented type is a typo, while json_object and
	// json_schema are the documented-but-unsupported case the table below owns.
	if raw, ok := req.Raw["response_format"]; ok {
		if _, err := outputFormatType(raw, "response_format"); err != nil {
			return err
		}
	}
	if err := validateUnsupportedFields(req.Raw, alwaysRejectChatFields); err != nil {
		return err
	}
	if err := validateMaxOutputTokens(req.MaxTokens, "max_tokens"); err != nil {
		return err
	}
	if err := validateMaxOutputTokens(req.MaxCompletionTokens, "max_completion_tokens"); err != nil {
		return err
	}
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
			return apierr.InvalidRequest(fmt.Sprintf("unsupported message role %q", msg.Role), fmt.Sprintf("messages.%d.role", i))
		}
		if msg.Role != "assistant" && len(msg.ToolCalls) > 0 {
			return apierr.InvalidRequest("tool_calls are only valid on assistant messages", fmt.Sprintf("messages.%d.tool_calls", i))
		}
		var err error
		switch msg.Role {
		case "tool":
			if msg.ToolCallID == "" {
				return apierr.InvalidRequest("tool messages require tool_call_id", fmt.Sprintf("messages.%d.tool_call_id", i))
			}
			// Tool results are data, not prose: LangChain's ToolMessage and MCP
			// bridges routinely carry a JSON object or array, so they get their own
			// branch instead of the text-only path.
			_, err = msg.Content.ToolOutput()
		case "user":
			_, err = msg.Prompt()
		default:
			_, err = msg.Text()
		}
		if err != nil {
			return apierr.InvalidRequest(err.Error(), fmt.Sprintf("messages.%d.content", i))
		}
	}
	return nil
}

func ValidateResponsesRequest(req *ResponsesRequest) error {
	if req.Model == "" {
		return apierr.InvalidRequest("model is required", "model")
	}
	if len(req.Input) == 0 || string(req.Input) == "null" {
		if req.PreviousResponseID == "" {
			return apierr.InvalidRequest("input is required", "input")
		}
	}
	if err := ValidateReasoningEffort(req.ReasoningEffort, "reasoning_effort"); err != nil {
		return err
	}
	if err := ValidateMetadata(req.Metadata); err != nil {
		return err
	}
	if err := validateUnsupportedFields(req.Raw, alwaysRejectResponseFields); err != nil {
		return err
	}
	// text.format decides whether the client can use the reply at all, so it is
	// checked here rather than left to the ignore-and-log path: a structured-output
	// request has to fail, and name the param it failed on.
	if err := validateResponsesText(req.Text); err != nil {
		return err
	}
	if err := validateResponsesMaxOutputTokens(req.Raw); err != nil {
		return err
	}
	if err := validateResponsesInclude(req.Include); err != nil {
		return err
	}
	if err := validateResponsesReasoning(req); err != nil {
		return err
	}
	if err := ValidateResponsesTools(req.Tools); err != nil {
		return err
	}
	if err := validateToolChoice(req.ToolChoice); err != nil {
		return err
	}
	return nil
}

// knownResponsesIncludeValues is OpenAI's documented include enum. Every entry
// asks for extra output detail, so an entry this proxy cannot produce is simply
// absent from the response rather than fatal. A value outside the enum is a typo
// and is rejected.
var knownResponsesIncludeValues = map[string]struct{}{
	"code_interpreter_call.outputs":         {},
	"computer_call_output.output.image_url": {},
	"file_search_call.results":              {},
	"message.input_image.image_url":         {},
	"message.output_text.logprobs":          {},
	"reasoning.encrypted_content":           {},
	"web_search_call.action.sources":        {},
	"web_search_call.results":               {},
}

func validateResponsesInclude(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return apierr.InvalidRequest("include must be an array of strings", "include")
	}
	for _, value := range values {
		if _, ok := knownResponsesIncludeValues[value]; !ok {
			return apierr.InvalidRequest("unknown include value", "include")
		}
	}
	return nil
}

// validateMaxOutputTokens accepts any positive cap. The Copilot SDK exposes no
// per-request output cap for CAPI sessions (only ProviderConfig.MaxOutputTokens,
// which applies to BYOK providers this proxy does not configure), so the value is
// accepted, surfaced through RequestedMaxOutputTokens, and logged at debug.
func validateMaxOutputTokens(value *int, param string) error {
	if value != nil && *value < 1 {
		return apierr.InvalidRequest(param+" must be a positive integer", param)
	}
	return nil
}

func validateResponsesMaxOutputTokens(raw map[string]json.RawMessage) error {
	value, ok := raw["max_output_tokens"]
	if !ok {
		return nil
	}
	var tokens int
	if err := json.Unmarshal(value, &tokens); err != nil || tokens < 1 {
		return apierr.InvalidRequest("max_output_tokens must be a positive integer", "max_output_tokens")
	}
	return nil
}

// RequestedMaxOutputTokens reports the output cap the client asked for,
// preferring max_completion_tokens over the deprecated max_tokens the way the
// OpenAI API does. Callers use it for diagnostics only; see
// validateMaxOutputTokens for why the value cannot be forwarded.
func (r *ChatCompletionRequest) RequestedMaxOutputTokens() (int, bool) {
	if r.MaxCompletionTokens != nil {
		return *r.MaxCompletionTokens, true
	}
	if r.MaxTokens != nil {
		return *r.MaxTokens, true
	}
	return 0, false
}

// RequestedLogprobs reports whether the client asked for token logprobs, via
// either logprobs:true or top_logprobs. They ask for an extra per-token
// annotation next to the reply, not for a different reply, and the Copilot SDK
// surfaces no token probabilities, so both are accepted and the gap is recorded
// at debug level by the HTTP layer instead of failing the request.
func (r *ChatCompletionRequest) RequestedLogprobs() bool {
	if raw, ok := r.Raw["logprobs"]; ok && !isFalse(raw) {
		return true
	}
	_, ok := r.Raw["top_logprobs"]
	return ok
}

// ResponsesMaxOutputTokens is the Responses counterpart of
// RequestedMaxOutputTokens.
func ResponsesMaxOutputTokens(req *ResponsesRequest) (int, bool) {
	raw, ok := req.Raw["max_output_tokens"]
	if !ok {
		return 0, false
	}
	var tokens int
	if err := json.Unmarshal(raw, &tokens); err != nil {
		return 0, false
	}
	return tokens, true
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
			return apierr.InvalidRequest("unsupported reasoning field", "reasoning."+name)
		}
	}
	if raw, ok := fields["effort"]; ok && string(raw) != "null" {
		var effort string
		if err := json.Unmarshal(raw, &effort); err != nil || effort == "" {
			return apierr.InvalidRequest("reasoning.effort must be a string", "reasoning.effort")
		}
		// The enum is checked before the conflict: a value outside it is wrong on
		// its own terms, whatever reasoning_effort says.
		if err := ValidateReasoningEffort(effort, "reasoning.effort"); err != nil {
			return err
		}
		if req.ReasoningEffort != "" && NormalizeReasoningEffort(req.ReasoningEffort) != NormalizeReasoningEffort(effort) {
			return apierr.InvalidRequest("reasoning.effort conflicts with reasoning_effort", "reasoning.effort")
		}
	}
	if raw, ok := fields["summary"]; ok && string(raw) != "null" {
		var summary string
		if err := json.Unmarshal(raw, &summary); err != nil {
			return apierr.InvalidRequest("reasoning.summary must be a string", "reasoning.summary")
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
			return apierr.InvalidRequest("unsupported text field", "text."+name)
		}
	}
	if raw, ok := fields["verbosity"]; ok && string(raw) != "null" {
		var verbosity string
		if err := json.Unmarshal(raw, &verbosity); err != nil || verbosity == "" {
			return apierr.InvalidRequest("text.verbosity must be a string", "text.verbosity")
		}
	}
	if raw, ok := fields["format"]; ok && string(raw) != "null" {
		if err := validateResponsesTextFormat(raw); err != nil {
			return err
		}
	}
	return nil
}

// validateResponsesTextFormat applies the structured-output clause to
// text.format. It is the Responses spelling of the rule the response_format
// entry of alwaysRejectChatFields applies on Chat Completions, down to the
// message, so a client that switches surfaces gets the same answer.
func validateResponsesTextFormat(raw json.RawMessage) error {
	formatType, err := outputFormatType(raw, "text.format")
	if err != nil {
		return err
	}
	if formatType != outputFormatText {
		return apierr.InvalidRequest(structuredOutputMessage("text.format"), "text.format")
	}
	return nil
}

// outputFormatText is the explicit default both surfaces spell the same way.
const outputFormatText = "text"

// outputFormatType reports the documented type of a Chat response_format or a
// Responses text.format object. Anything outside OpenAI's enum is a typo and is
// rejected on the nested .type param; deciding what to do with a documented type
// is the caller's job.
func outputFormatType(raw json.RawMessage, param string) (string, error) {
	fields, err := rawObject(raw, param)
	if err != nil {
		return "", err
	}
	var formatType string
	if err := json.Unmarshal(fields["type"], &formatType); err != nil || formatType == "" {
		return "", apierr.InvalidRequest(param+".type must be a string", param+".type")
	}
	switch formatType {
	case outputFormatText, "json_object", "json_schema":
		return formatType, nil
	default:
		return "", apierr.InvalidRequest("unknown "+param+".type", param+".type")
	}
}

// structuredOutputMessage is the single wording both surfaces use to turn down
// json_object and json_schema. It names the feature rather than the payload:
// the request is well formed, the backend simply cannot constrain model output,
// and accepting it would return prose to a caller that is about to parse JSON.
func structuredOutputMessage(param string) string {
	return "structured output is not supported by this backend: the Copilot SDK exposes no response-format control, so a schema could not be enforced and the model would return free-form text; send " + param + ` {"type":"text"} and parse the reply yourself`
}

func rawObject(raw json.RawMessage, param string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, apierr.InvalidRequest(param+" must be an object", param)
	}
	return fields, nil
}

func ResponsesReasoningEffort(req *ResponsesRequest) string {
	if req.ReasoningEffort != "" {
		return NormalizeReasoningEffort(req.ReasoningEffort)
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
	return NormalizeReasoningEffort(effort)
}

func validateUnsupportedFields(raw map[string]json.RawMessage, fields []unsupportedField) error {
	for _, field := range fields {
		value, ok := raw[field.name]
		if !ok {
			continue
		}
		if field.allow != nil && field.allow(value) {
			continue
		}
		return apierr.InvalidRequest(field.message, field.name)
	}
	return nil
}

func ValidateTools(tools []Tool) error {
	return validateTools(tools, false)
}

func ValidateResponsesTools(tools []Tool) error {
	_, err := NormalizeResponsesTools(tools)
	return err
}

func validateTools(tools []Tool, allowUnsupported bool) error {
	seen := map[string]struct{}{}
	for i, tool := range tools {
		if tool.Type != "function" {
			if allowUnsupported {
				continue
			}
			return apierr.InvalidRequest("only function tools are supported", fmt.Sprintf("tools.%d.type", i))
		}
		fn := tool.Function
		if !functionNameRE.MatchString(fn.Name) {
			return apierr.InvalidRequest("function tool name must match ^[A-Za-z0-9_-]{1,64}$", fmt.Sprintf("tools.%d.function.name", i))
		}
		if _, ok := seen[fn.Name]; ok {
			return apierr.InvalidRequest("duplicate function tool name", fmt.Sprintf("tools.%d.function.name", i))
		}
		seen[fn.Name] = struct{}{}
		if len(fn.Parameters) > 0 {
			var js any
			if err := json.Unmarshal(fn.Parameters, &js); err != nil {
				return apierr.InvalidRequest("function parameters must be valid JSON Schema", fmt.Sprintf("tools.%d.function.parameters", i))
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

// ToolChoice is a decoded tool_choice value.
type ToolChoice struct {
	// Kind is "", "auto", "none", "required", "function", "custom", or
	// "allowed_tools".
	Kind string
	// Name is the forced tool name for the "function" and "custom" kinds.
	Name string
}

// Honored reports whether the Copilot SDK can enforce this choice. "auto" is
// the backend's own behavior and "none" is emulated by withholding the tool
// catalog; the forcing modes have no SDK equivalent, so they are accepted and
// logged at debug instead of rejected. OpenAI's Structured Outputs guidance
// alone makes rejecting them untenable.
func (c ToolChoice) Honored() bool {
	return c.Kind == "" || c.Kind == "auto" || c.Kind == "none"
}

// ParseToolChoice decodes tool_choice, rejecting only values OpenAI does not
// define.
func ParseToolChoice(raw json.RawMessage) (ToolChoice, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return ToolChoice{}, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto", "none", "required":
			return ToolChoice{Kind: s}, nil
		default:
			return ToolChoice{}, apierr.InvalidRequest("tool_choice must be auto, none, required, or a tool object", "tool_choice")
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ToolChoice{}, apierr.InvalidRequest("tool_choice must be auto, none, required, or a tool object", "tool_choice")
	}
	switch obj.Type {
	case "function", "custom":
		// Chat Completions nests the name under "function"; Responses puts it at
		// the top level. Both spellings reach this proxy.
		name := obj.Name
		if name == "" {
			name = obj.Function.Name
		}
		if name == "" {
			return ToolChoice{}, apierr.InvalidRequest("forced tool_choice requires a tool name", "tool_choice")
		}
		return ToolChoice{Kind: obj.Type, Name: name}, nil
	case "allowed_tools":
		return ToolChoice{Kind: obj.Type}, nil
	default:
		return ToolChoice{}, apierr.InvalidRequest("unsupported tool_choice", "tool_choice")
	}
}

func validateToolChoice(raw json.RawMessage) error {
	_, err := ParseToolChoice(raw)
	return err
}

func ToolChoiceNone(raw json.RawMessage) bool {
	var s string
	return len(raw) > 0 && json.Unmarshal(raw, &s) == nil && s == "none"
}

func isOne(raw json.RawMessage) bool {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return false
	}
	value, err := n.Float64()
	return err == nil && value == 1
}

func isFalse(raw json.RawMessage) bool {
	var b bool
	return json.Unmarshal(raw, &b) == nil && !b
}

// isDefaultOutputFormat is the allow predicate for Chat response_format and the
// nested half of isDefaultTextObject: {"type":"text"} asks for exactly what this
// proxy already returns, so honouring it is a no-op rather than a silent
// downgrade. json_object and json_schema fall through to the table message.
func isDefaultOutputFormat(raw json.RawMessage) bool {
	var format struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &format) == nil && format.Type == outputFormatText
}

// FoldChatInstructions hoists the leading system/developer messages into the
// session instructions, preserving their order.
//
// A system/developer message that arrives after the conversation has started
// (LangGraph injections between tool rounds, Cline/Roo Code "system reminders")
// cannot be folded: the instructions are the session's opening system message,
// so hoisting a mid-conversation reminder would reorder it relative to the turn
// it annotates and make it permanent. Those messages are spliced into the
// transcript instead, as the same "System:"/"Developer:" blocks the Responses
// surface renders in internal/httpapi/responses_input.go. Either way nothing is
// dropped and nothing is rejected on position alone — which is what
// ValidateChatRequest has always assumed by accepting these roles anywhere.
func FoldChatInstructions(messages []ChatMessage) (string, []ChatMessage, error) {
	var instructions []string
	out := make([]ChatMessage, 0, len(messages))
	leading := true
	for i, msg := range messages {
		if msg.Role != "system" && msg.Role != "developer" {
			leading = false
			out = append(out, msg)
			continue
		}
		text, err := msg.Text()
		if err != nil {
			return "", nil, apierr.InvalidRequest(err.Error(), fmt.Sprintf("messages.%d.content", i))
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		block := chatInstructionLabel(msg.Role) + ":\n" + text
		if leading {
			instructions = append(instructions, block)
			continue
		}
		out = append(out, ChatMessage{Role: "user", Content: NewTextContent(block)})
	}
	return strings.Join(instructions, "\n\n"), out, nil
}

func chatInstructionLabel(role string) string {
	if role == "developer" {
		return "Developer"
	}
	return "System"
}

func InstructionCandidates(s string) []string {
	if s != "" {
		return []string{s}
	}
	return []string{" ", "You are a chat completion model."}
}

// OpenAI's metadata limits: at most 16 pairs, keys up to 64 characters and
// values up to 512.
const (
	maxMetadataPairs    = 16
	maxMetadataKeyLen   = 64
	maxMetadataValueLen = 512
)

// ValidateMetadata bounds the client's own key/value tagging.
//
// metadata is round-trippable state - the real API echoes it on the response
// object and on GET /v1/responses/{id} - so this proxy stores and echoes it
// rather than dropping it. Accepting it and discarding it would be the silent
// acceptance the validation policy rules out, and it degrades badly: a client
// tagging responses with a trace id gets 200 OK and then finds the field gone
// on read, with nothing to indicate why.
//
// The limits are rejected rather than truncated. Truncating a trace id yields a
// value that looks valid and correlates to nothing.
func ValidateMetadata(metadata map[string]string) error {
	if len(metadata) > maxMetadataPairs {
		return apierr.InvalidRequest(fmt.Sprintf("metadata supports at most %d key-value pairs, got %d", maxMetadataPairs, len(metadata)), "metadata")
	}
	for key, value := range metadata {
		if len(key) > maxMetadataKeyLen {
			return apierr.InvalidRequest(fmt.Sprintf("metadata keys support at most %d characters, got %d for %q", maxMetadataKeyLen, len(key), key), "metadata")
		}
		if len(value) > maxMetadataValueLen {
			return apierr.InvalidRequest(fmt.Sprintf("metadata values support at most %d characters, got %d for key %q", maxMetadataValueLen, len(value), key), "metadata")
		}
	}
	return nil
}
