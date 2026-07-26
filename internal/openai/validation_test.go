package openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestResponseTextMarshalsEmptyAnnotationsArray(t *testing.T) {
	data, err := json.Marshal(ResponseText{Type: "output_text", Text: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), `{"type":"output_text","text":"ok","annotations":[]}`; got != want {
		t.Fatalf("ResponseText JSON = %s, want %s", got, want)
	}
}

func TestInstructionCandidatesAvoidEmptySystemMessage(t *testing.T) {
	got := InstructionCandidates("")
	if len(got) == 0 || got[0] == "" {
		t.Fatalf("InstructionCandidates(empty) = %#v, want first candidate to be non-empty", got)
	}
	if got[0] != " " {
		t.Fatalf("InstructionCandidates(empty)[0] = %q, want single-space replacement", got[0])
	}
}

func TestFoldChatInstructionsSplicesMidConversationSystemMessages(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: NewTextContent("sys")},
		{Role: "user", Content: NewTextContent("hi")},
		{Role: "developer", Content: NewTextContent("late")},
		{Role: "user", Content: NewTextContent("again")},
	}
	instructions, rest, err := FoldChatInstructions(msgs)
	if err != nil {
		t.Fatalf("mid-conversation developer message must not be rejected: %v", err)
	}
	if instructions != "System:\nsys" {
		t.Fatalf("instructions = %q, want only the leading system message", instructions)
	}
	if len(rest) != 3 {
		t.Fatalf("messages = %#v, want three transcript messages", rest)
	}
	text, err := rest[1].Text()
	if err != nil {
		t.Fatal(err)
	}
	if rest[1].Role != "user" || text != "Developer:\nlate" {
		t.Fatalf("spliced message = %q/%q, want a user-role Developer block", rest[1].Role, text)
	}
}

func TestFoldChatInstructionsKeepsLeadingRunOrder(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: NewTextContent("first")},
		{Role: "developer", Content: NewTextContent("second")},
		{Role: "user", Content: NewTextContent("hi")},
	}
	instructions, rest, err := FoldChatInstructions(msgs)
	if err != nil {
		t.Fatal(err)
	}
	if instructions != "System:\nfirst\n\nDeveloper:\nsecond" {
		t.Fatalf("instructions = %q, want both leading blocks in order", instructions)
	}
	if len(rest) != 1 || rest[0].Role != "user" {
		t.Fatalf("messages = %#v, want only the user message", rest)
	}
}

func TestValidateToolChoice(t *testing.T) {
	tests := []struct {
		name     string
		choice   string
		accepted bool
	}{
		{name: "auto", choice: `"auto"`, accepted: true},
		{name: "none", choice: `"none"`, accepted: true},
		{name: "required", choice: `"required"`, accepted: true},
		{name: "chat forced function", choice: `{"type":"function","function":{"name":"lookup"}}`, accepted: true},
		{name: "responses forced function", choice: `{"type":"function","name":"lookup"}`, accepted: true},
		{name: "allowed tools", choice: `{"mode":"auto","type":"allowed_tools","tools":[]}`, accepted: true},
		{name: "unknown mode", choice: `"any"`, accepted: false},
		{name: "unknown type", choice: `{"type":"web_search"}`, accepted: false},
		{name: "forced function without a name", choice: `{"type":"function"}`, accepted: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := decodeChatRequest(t, `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tool_choice":`+tt.choice+`}`)
			err := ValidateChatRequest(req, true)
			if tt.accepted && err != nil {
				t.Fatalf("tool_choice %s should be accepted: %v", tt.choice, err)
			}
			if !tt.accepted && err == nil {
				t.Fatalf("tool_choice %s should be rejected", tt.choice)
			}
		})
	}
}

// The forcing modes are accepted but cannot be enforced by the Copilot SDK, so
// the HTTP layer needs Honored to decide what to report at debug level.
func TestToolChoiceHonored(t *testing.T) {
	tests := map[string]bool{
		``:                                       true,
		`"auto"`:                                 true,
		`"none"`:                                 true,
		`"required"`:                             false,
		`{"type":"function","name":"lookup"}`:    false,
		`{"type":"custom","name":"apply_patch"}`: false,
		`{"type":"allowed_tools","mode":"auto"}`: false,
	}
	for raw, want := range tests {
		choice, err := ParseToolChoice(json.RawMessage(raw))
		if err != nil {
			t.Fatalf("ParseToolChoice(%s) = %v", raw, err)
		}
		if got := choice.Honored(); got != want {
			t.Fatalf("ParseToolChoice(%s).Honored() = %t, want %t", raw, got, want)
		}
	}
	if name := mustParseToolChoice(t, `{"type":"function","function":{"name":"lookup"}}`).Name; name != "lookup" {
		t.Fatalf("nested forced tool name = %q, want lookup", name)
	}
}

func mustParseToolChoice(t *testing.T, raw string) ToolChoice {
	t.Helper()
	choice, err := ParseToolChoice(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return choice
}

// Every one of these is a valid OpenAI request that a widely-used client sends
// by default, so permissive mode (and here even strict mode) must accept it.
func TestValidateChatAcceptsCommonClientParameters(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "max_tokens", body: `{"model":"gpt-5","max_tokens":256,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "max_completion_tokens", body: `{"model":"gpt-5","max_completion_tokens":256,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "parallel_tool_calls false", body: `{"model":"gpt-5","parallel_tool_calls":false,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "tool_choice required", body: `{"model":"gpt-5","tool_choice":"required","messages":[{"role":"user","content":"hi"}]}`},
		{name: "logprobs false", body: `{"model":"gpt-5","logprobs":false,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "stream false", body: `{"model":"gpt-5","stream":false,"messages":[{"role":"user","content":"hi"}]}`},
		{name: "digit leading tool name", body: `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"2fa_verify"}}]}`},
		{name: "mid conversation system message", body: `{"model":"gpt-5","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"},{"role":"system","content":"reminder"},{"role":"user","content":"again"}]}`},
		{name: "object tool result", body: `{"model":"gpt-5","messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":{"answer":42}}]}`},
		{name: "array tool result", body: `{"model":"gpt-5","messages":[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":[{"row":1}]}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := decodeChatRequest(t, tt.body)
			assertChatAccepted(t, req, false)
			assertChatAccepted(t, req, true)
		})
	}
}

func TestValidateChatRejectsInvalidMaxTokens(t *testing.T) {
	for _, param := range []string{"max_tokens", "max_completion_tokens"} {
		assertChatRejected(t, decodeChatRequest(t, chatBody(param, `0`)), false, param)
		assertChatRejected(t, decodeChatRequest(t, chatBody(param, `-1`)), false, param)
	}
}

func TestValidateChatRejectsUnusableToolNames(t *testing.T) {
	for _, name := range []string{"", "has space", "dotted.name", strings.Repeat("a", 65)} {
		req := decodeChatRequest(t, `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"`+name+`"}}]}`)
		if err := ValidateChatRequest(req, false); err == nil {
			t.Fatalf("function tool name %q should be rejected", name)
		}
	}
}

func TestValidateResponsesAcceptsCommonClientParameters(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "max_output_tokens", body: `{"model":"gpt-5","max_output_tokens":256,"input":"hi"}`},
		{name: "parallel_tool_calls false", body: `{"model":"gpt-5","parallel_tool_calls":false,"input":"hi"}`},
		{name: "tool_choice required", body: `{"model":"gpt-5","tool_choice":"required","input":"hi"}`},
		{name: "explicit default text format", body: `{"model":"gpt-5","text":{"format":{"type":"text"}},"input":"hi"}`},
		{name: "unsupported but documented include", body: `{"model":"gpt-5","include":["file_search_call.results"],"input":"hi"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertResponsesAccepted(t, decodeResponsesRequest(t, tt.body), false)
		})
	}
}

func TestValidateResponsesRejectsInvalidMaxOutputTokens(t *testing.T) {
	assertResponsesRejected(t, decodeResponsesRequest(t, responsesBody("max_output_tokens", `0`)), false, "max_output_tokens")
	assertResponsesRejected(t, decodeResponsesRequest(t, responsesBody("max_output_tokens", `"lots"`)), false, "max_output_tokens")
}

func TestChatPermissiveAllowsIgnoredSamplingFields(t *testing.T) {
	var req ChatCompletionRequest
	body := []byte(`{"model":"gpt-5","temperature":0.1,"messages":[{"role":"user","content":"hi"}]}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateChatRequest(&req, false); err != nil {
		t.Fatalf("temperature should be accepted in permissive mode: %v", err)
	}
	if err := ValidateChatRequest(&req, true); err == nil {
		t.Fatal("expected temperature rejection in strict mode")
	}
}

func TestPermissiveChatRejectsUnsafeUnsupportedFields(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		param string
	}{
		{
			name:  "response format",
			body:  `{"model":"gpt-5","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"hi"}]}`,
			param: "response_format",
		},
		{
			name:  "stop",
			body:  `{"model":"gpt-5","stop":["done"],"messages":[{"role":"user","content":"hi"}]}`,
			param: "stop",
		},
		{
			name:  "n greater than one",
			body:  `{"model":"gpt-5","n":2,"messages":[{"role":"user","content":"hi"}]}`,
			param: "n",
		},
		{
			name:  "logprobs true",
			body:  `{"model":"gpt-5","logprobs":true,"messages":[{"role":"user","content":"hi"}]}`,
			param: "logprobs",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req ChatCompletionRequest
			if err := json.Unmarshal([]byte(tt.body), &req); err != nil {
				t.Fatal(err)
			}
			err := ValidateChatRequest(&req, false)
			if err == nil {
				t.Fatal("expected unsafe field rejection in permissive mode")
			}
			apiErr, ok := err.(*APIError)
			if !ok || apiErr.Param != tt.param {
				t.Fatalf("error = %#v, want param %q", err, tt.param)
			}
		})
	}
}

func TestPermissiveChatAllowsSingleChoiceN(t *testing.T) {
	var req ChatCompletionRequest
	body := []byte(`{"model":"gpt-5","n":1,"messages":[{"role":"user","content":"hi"}]}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateChatRequest(&req, false); err != nil {
		t.Fatalf("n=1 should be accepted in permissive mode: %v", err)
	}
	if err := ValidateChatRequest(&req, true); err != nil {
		t.Fatalf("n=1 should be accepted in strict mode: %v", err)
	}
}

func TestPermissiveResponsesRejectsUnsafeUnsupportedFields(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		param string
	}{
		{
			name:  "unknown text format type",
			body:  `{"model":"gpt-5","text":{"format":{"type":"jsonschema"}},"input":"hi"}`,
			param: "text.format.type",
		},
		{
			name:  "unknown reasoning field",
			body:  `{"model":"gpt-5","reasoning":{"foo":"bar"},"input":"hi"}`,
			param: "reasoning.foo",
		},
		{
			name:  "unknown include value",
			body:  `{"model":"gpt-5","include":["reasoning.encryped_content"],"input":"hi"}`,
			param: "include",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req ResponsesRequest
			if err := json.Unmarshal([]byte(tt.body), &req); err != nil {
				t.Fatal(err)
			}
			err := ValidateResponsesRequest(&req, false)
			if err == nil {
				t.Fatal("expected unsafe field rejection in permissive mode")
			}
			apiErr, ok := err.(*APIError)
			if !ok || apiErr.Param != tt.param {
				t.Fatalf("error = %#v, want param %q", err, tt.param)
			}
		})
	}
}

// Structured output is the one documented-but-unsupported control that cannot be
// accepted and ignored: the client is about to JSON.parse the reply, so prose
// fails far from its cause or parses into silently wrong data. Chat
// response_format and Responses text.format therefore have to answer the same
// way, in both modes, for every documented value.
func TestOutputFormatAcceptsOnlyTheExplicitTextDefault(t *testing.T) {
	tests := []struct {
		name string
		// format is the response_format / text.format object; empty omits it.
		format string
		// chatParam and responsesParam are empty when the request is accepted.
		chatParam      string
		responsesParam string
	}{
		{name: "omitted"},
		{name: "explicit default", format: `{"type":"text"}`},
		{
			name:           "json_schema",
			format:         `{"type":"json_schema","name":"out","schema":{"type":"object"}}`,
			chatParam:      "response_format",
			responsesParam: "text.format",
		},
		{
			name:           "json_object",
			format:         `{"type":"json_object"}`,
			chatParam:      "response_format",
			responsesParam: "text.format",
		},
		{
			name:           "unknown type",
			format:         `{"type":"jsonschema"}`,
			chatParam:      "response_format.type",
			responsesParam: "text.format.type",
		},
		{
			name:           "missing type",
			format:         `{"name":"out"}`,
			chatParam:      "response_format.type",
			responsesParam: "text.format.type",
		},
		{
			name:           "not an object",
			format:         `"json_schema"`,
			chatParam:      "response_format",
			responsesParam: "text.format",
		},
	}
	for _, tt := range tests {
		for _, strict := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/strict=%t", tt.name, strict), func(t *testing.T) {
				chatBodyJSON := `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`
				responsesBodyJSON := `{"model":"gpt-5","input":"hi"}`
				if tt.format != "" {
					chatBodyJSON = chatBody("response_format", tt.format)
					responsesBodyJSON = responsesBody("text", `{"format":`+tt.format+`}`)
				}
				chat := decodeChatRequest(t, chatBodyJSON)
				responses := decodeResponsesRequest(t, responsesBodyJSON)
				if tt.chatParam == "" {
					assertChatAccepted(t, chat, strict)
					assertResponsesAccepted(t, responses, strict)
					return
				}
				assertChatRejected(t, chat, strict, tt.chatParam)
				assertResponsesRejected(t, responses, strict, tt.responsesParam)

				// The two surfaces must explain the refusal identically, otherwise a
				// client that switches surfaces learns two different things.
				chatMessage := ValidateChatRequest(chat, strict).Error()
				responsesMessage := ValidateResponsesRequest(responses, strict).Error()
				if got := strings.ReplaceAll(chatMessage, "response_format", "text.format"); got != responsesMessage {
					t.Fatalf("messages diverge:\n chat: %s\n responses: %s", chatMessage, responsesMessage)
				}
				if tt.chatParam == "response_format" && tt.format != `"json_schema"` && !strings.Contains(chatMessage, "structured output is not supported") {
					t.Fatalf("message = %q, want it to name the unsupported feature", chatMessage)
				}
			})
		}
	}
}

func TestResponsesInputMayBeOmittedForPreviousResponseContinuation(t *testing.T) {
	var req ResponsesRequest
	body := []byte(`{"model":"gpt-5","previous_response_id":"resp_previous"}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResponsesRequest(&req, false); err != nil {
		t.Fatalf("missing input should be accepted with previous_response_id: %v", err)
	}
}

func TestResponsesPermissiveAllowsIgnoredFields(t *testing.T) {
	var req ResponsesRequest
	body := []byte(`{"model":"gpt-5","temperature":0.1,"top_p":0.9,"input":"hi"}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResponsesRequest(&req, false); err != nil {
		t.Fatalf("ignored fields should be accepted in permissive mode: %v", err)
	}
	if err := ValidateResponsesRequest(&req, true); err == nil {
		t.Fatal("expected ignored fields to be rejected in strict mode")
	}
}

func TestResponsesAcceptsCodexReasoningDefaultsAndClientOwnedTools(t *testing.T) {
	var req ResponsesRequest
	body := []byte(`{"model":"gpt-5.5","include":["reasoning.encrypted_content"],"reasoning":{"effort":"medium","summary":"auto"},"text":{"verbosity":"low"},"tools":[{"type":"function","name":"exec_command","description":"run","parameters":{"type":"object","properties":{}}},{"type":"function","name":"multi_tool_use.parallel","description":"parallel","parameters":{"type":"object","properties":{}}},{"type":"custom","name":"apply_patch","description":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},{"type":"namespace","name":"mcp__grep_app","tools":[{"name":"searchGitHub","description":"search","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}]},{"type":"tool_search","execution":"client","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}],"input":"hi"}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResponsesRequest(&req, false); err != nil {
		t.Fatalf("Codex reasoning defaults and Responses tools should be accepted in permissive mode: %v", err)
	}
	var strictReq ResponsesRequest
	strictBody := []byte(`{"model":"gpt-5.5","tools":[{"type":"function","name":"exec_command","description":"run","parameters":{"type":"object","properties":{}}},{"type":"function","name":"multi_tool_use.parallel","description":"parallel","parameters":{"type":"object","properties":{}}},{"type":"custom","name":"apply_patch","description":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},{"type":"namespace","name":"mcp__grep_app","tools":[{"name":"searchGitHub","description":"search","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}]},{"type":"tool_search","execution":"client","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}],"input":"hi"}`)
	if err := json.Unmarshal(strictBody, &strictReq); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResponsesRequest(&strictReq, true); err != nil {
		t.Fatalf("supported Responses tools should be accepted in strict mode: %v", err)
	}
	if got := ResponsesReasoningEffort(&req); got != "medium" {
		t.Fatalf("ResponsesReasoningEffort = %q, want medium", got)
	}
	normalized, err := NormalizeResponsesTools(req.Tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 5 || normalized[2].Kind != ToolKindCustom || normalized[3].Kind != ToolKindNamespace || normalized[4].Kind != ToolKindToolSearch {
		t.Fatalf("normalized tools = %#v, want function/function/custom/namespace/tool_search", normalized)
	}
}

func TestResponsesIgnoresHostedToolsInPermissiveAndRejectsInStrict(t *testing.T) {
	var req ResponsesRequest
	body := []byte(`{"model":"gpt-5.5","tools":[{"type":"web_search","external_web_access":true}],"input":"hi"}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResponsesRequest(&req, false); err != nil {
		t.Fatalf("hosted web_search should be ignored in permissive mode: %v", err)
	}
	normalized, err := NormalizeResponsesToolsWithMode(req.Tools, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 0 {
		t.Fatalf("normalized hosted tools = %#v, want none", normalized)
	}
	if got := IgnoredResponsesToolTypes(req.Tools); len(got) != 1 || got[0] != "web_search" {
		t.Fatalf("IgnoredResponsesToolTypes = %#v, want [web_search] so the drop can be logged", got)
	}
	if err := ValidateResponsesRequest(&req, true); err == nil {
		t.Fatal("expected hosted web_search rejection in strict mode")
	}
}

// A tool type that is neither supported nor a known hosted tool is a client
// typo. Dropping it silently would ship a request with a missing capability, so
// it is a 400 in both modes.
func TestResponsesRejectsUnknownToolTypeInBothModes(t *testing.T) {
	var req ResponsesRequest
	body := []byte(`{"model":"gpt-5.5","tools":[{"type":"funcion","name":"lookup"}],"input":"hi"}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	for _, strict := range []bool{false, true} {
		err := ValidateResponsesRequest(&req, strict)
		apiErr, ok := err.(*APIError)
		if !ok || apiErr.Param != "tools.0.type" {
			t.Fatalf("ValidateResponsesRequest(strict=%t) = %#v, want a 400 on tools.0.type", strict, err)
		}
	}
	if got := IgnoredResponsesToolTypes(req.Tools); len(got) != 0 {
		t.Fatalf("IgnoredResponsesToolTypes = %#v, want none for an unknown type", got)
	}
}

func TestValidateChatRejectsUnsupportedToolTypes(t *testing.T) {
	var req ChatCompletionRequest
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","name":"apply_patch"}]}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateChatRequest(&req, false); err == nil {
		t.Fatal("expected Chat custom tools to be rejected")
	}
}

func TestNewResponseUsageUsesResponsesTokenNames(t *testing.T) {
	prompt := int64(3)
	completion := int64(5)
	reasoning := int64(2)
	usage := NewResponseUsage(&Usage{PromptTokens: &prompt, CompletionTokens: &completion, CompletionTokensDetails: &TokenDetails{ReasoningTokens: &reasoning}})
	b, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{`"input_tokens":3`, `"output_tokens":5`, `"total_tokens":8`, `"reasoning_tokens":2`} {
		if !strings.Contains(got, want) {
			t.Fatalf("usage JSON missing %s: %s", want, got)
		}
	}
	if strings.Contains(got, "prompt_tokens") || strings.Contains(got, "completion_tokens") {
		t.Fatalf("usage JSON should use Responses field names: %s", got)
	}
}

func TestNewResponseUsageOmitsReasoningOnlyUsage(t *testing.T) {
	reasoning := int64(2)
	if usage := NewResponseUsage(&Usage{CompletionTokensDetails: &TokenDetails{ReasoningTokens: &reasoning}}); usage != nil {
		t.Fatalf("reasoning-only usage = %#v, want nil", usage)
	}
}

func TestResponseUsageUnmarshalsLegacyChatUsage(t *testing.T) {
	var usage ResponseUsage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8,"completion_tokens_details":{"reasoning_tokens":2}}`), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens == nil || *usage.InputTokens != 3 || usage.OutputTokens == nil || *usage.OutputTokens != 5 || usage.TotalTokens == nil || *usage.TotalTokens != 8 {
		t.Fatalf("legacy usage was not migrated: %#v", usage)
	}
	if usage.OutputTokensDetails == nil || usage.OutputTokensDetails.ReasoningTokens == nil || *usage.OutputTokensDetails.ReasoningTokens != 2 {
		t.Fatalf("legacy reasoning tokens were not migrated: %#v", usage.OutputTokensDetails)
	}
}

func TestContentTextRejectsImages(t *testing.T) {
	var c Content
	if err := json.Unmarshal([]byte(`[{"type":"input_image","image_url":"x"}]`), &c); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Text(); err == nil {
		t.Fatal("expected unsupported image part error")
	}
	prompt, err := c.Prompt()
	if err != nil {
		t.Fatal(err)
	}
	if len(prompt.Images) != 1 || prompt.Images[0].URL != "x" {
		t.Fatalf("expected one parsed image, got %#v", prompt.Images)
	}
}

func TestValidateChatAllowsUserImageParts(t *testing.T) {
	var req ChatCompletionRequest
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA","detail":"low"}}]}]}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateChatRequest(&req, true); err != nil {
		t.Fatalf("user image content should be accepted: %v", err)
	}
	prompt, err := req.Messages[0].Prompt()
	if err != nil {
		t.Fatal(err)
	}
	if prompt.Text != "describe" || len(prompt.Images) != 1 || prompt.Images[0].Detail != "low" {
		t.Fatalf("unexpected prompt parse: %#v", prompt)
	}
}

func TestValidateChatRejectsAssistantImageParts(t *testing.T) {
	var req ChatCompletionRequest
	body := []byte(`{"model":"gpt-5","messages":[{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateChatRequest(&req, true); err == nil {
		t.Fatal("expected assistant image content rejection")
	}
}
