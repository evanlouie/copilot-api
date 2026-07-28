package openai

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
)

func TestResponseTextMarshalsEmptyAnnotationsArray(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(ResponseText{Type: "output_text", Text: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), `{"type":"output_text","text":"ok","annotations":[]}`; got != want {
		t.Fatalf("ResponseText JSON = %s, want %s", got, want)
	}
}

func TestInstructionCandidatesAvoidEmptySystemMessage(t *testing.T) {
	t.Parallel()
	got := InstructionCandidates("")
	if len(got) == 0 || got[0] == "" {
		t.Fatalf("InstructionCandidates(empty) = %#v, want first candidate to be non-empty", got)
	}
	if got[0] != " " {
		t.Fatalf("InstructionCandidates(empty)[0] = %q, want single-space replacement", got[0])
	}
}

func TestFoldChatInstructionsSplicesMidConversationSystemMessages(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
		{name: "allowed tools", choice: `{"mode":"auto","type":"allowed_tools","tools":[{"name":"lookup"}]}`, accepted: true},
		{name: "allowed tools missing mode", choice: `{"type":"allowed_tools","tools":[{"name":"lookup"}]}`, accepted: false},
		{name: "allowed tools invalid mode", choice: `{"type":"allowed_tools","mode":"requird","tools":[{"name":"lookup"}]}`, accepted: false},
		{name: "allowed tools missing tools", choice: `{"type":"allowed_tools","mode":"auto"}`, accepted: false},
		{name: "allowed tools null tools", choice: `{"type":"allowed_tools","mode":"auto","tools":null}`, accepted: false},
		{name: "allowed tools empty tools", choice: `{"type":"allowed_tools","mode":"auto","tools":[]}`, accepted: false},
		{name: "chat allowed tools missing nested tools", choice: `{"type":"allowed_tools","allowed_tools":{"mode":"auto"}}`, accepted: false},
		{name: "unknown mode", choice: `"any"`, accepted: false},
		{name: "unknown type", choice: `{"type":"web_search"}`, accepted: false},
		{name: "forced function without a name", choice: `{"type":"function"}`, accepted: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := decodeChatRequest(t, `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tool_choice":`+tt.choice+`}`)
			err := ValidateChatRequest(req)
			if tt.accepted && err != nil {
				t.Fatalf("tool_choice %s should be accepted: %v", tt.choice, err)
			}
			if !tt.accepted && err == nil {
				t.Fatalf("tool_choice %s should be rejected", tt.choice)
			}
		})
	}
}

// Narrowing the tool catalog is all the enforcement the Copilot SDK allows, so
// the HTTP layer needs Honored to decide which part of a choice is worth
// reporting at debug level: the demand to actually call a tool, never the
// allow-list that catalog filtering satisfies exactly.
func TestToolChoiceHonored(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		``:                                       true,
		`"auto"`:                                 true,
		`"none"`:                                 true,
		`"required"`:                             false,
		`{"type":"function","name":"lookup"}`:    false,
		`{"type":"custom","name":"apply_patch"}`: false,
		`{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"lookup"}]}`:     true,
		`{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"lookup"}]}`: false,
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

// The shape and enum are part of OpenAI's grammar. Guessing that an absent
// list means "none", or treating a typo as auto, silently removes tools the
// client may have intended to expose.
func TestParseToolChoiceRejectsMalformedAllowedTools(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"missing mode":         `{"type":"allowed_tools","tools":[{"name":"lookup"}]}`,
		"unknown mode":         `{"type":"allowed_tools","mode":"Required","tools":[{"name":"lookup"}]}`,
		"missing tools":        `{"type":"allowed_tools","mode":"auto"}`,
		"null tools":           `{"type":"allowed_tools","mode":"auto","tools":null}`,
		"empty tools":          `{"type":"allowed_tools","mode":"auto","tools":[]}`,
		"nested missing tools": `{"type":"allowed_tools","allowed_tools":{"mode":"required"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := ParseToolChoice(json.RawMessage(raw))
			if err == nil {
				t.Fatalf("ParseToolChoice(%s) accepted malformed allowed_tools", raw)
			}
			if !strings.Contains(err.Error(), "allowed_tools") {
				t.Fatalf("error = %v, want it to name allowed_tools", err)
			}
		})
	}
}

// The allow-list is what makes allowed_tools enforceable by catalog filtering,
// so it has to survive parsing from both the Chat Completions nesting and the
// flat Responses one.
func TestParseToolChoiceReadsAllowedToolsFromBothSpellings(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"responses": `{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"lookup"},{"type":"custom","name":"apply_patch"}]}`,
		"chat":      `{"type":"allowed_tools","allowed_tools":{"mode":"required","tools":[{"type":"function","function":{"name":"lookup"}},{"type":"custom","name":"apply_patch"}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			choice := mustParseToolChoice(t, raw)
			if choice.AllowedMode != "required" {
				t.Fatalf("AllowedMode = %q, want required", choice.AllowedMode)
			}
			if !slices.Equal(choice.Allowed, []string{"lookup", "apply_patch"}) {
				t.Fatalf("Allowed = %#v, want the declared order", choice.Allowed)
			}
		})
	}
}

// An unnameable entry would be dropped from the allow-list, which withholds a
// tool the client permitted. That is exactly the silent downgrade validation
// exists to prevent.
func TestParseToolChoiceRejectsUnnamedAllowedTool(t *testing.T) {
	t.Parallel()
	_, err := ParseToolChoice(json.RawMessage(`{"type":"allowed_tools","mode":"auto","tools":[{"type":"function"}]}`))
	if err == nil || !strings.Contains(err.Error(), "allowed_tools entries require a tool name") {
		t.Fatalf("error = %v, want a named-entry rejection", err)
	}
}

// Scope is the whole of what this proxy can enforce, so each choice has to map
// onto the catalog it is meant to leave the model with.
func TestToolChoiceScope(t *testing.T) {
	t.Parallel()
	tests := map[string]ToolScope{
		``:                                       {},
		`"auto"`:                                 {},
		`"required"`:                             {},
		`"none"`:                                 {None: true},
		`{"type":"function","name":"lookup"}`:    {Only: []string{"lookup"}, Kind: "function", Forced: true},
		`{"type":"custom","name":"apply_patch"}`: {Only: []string{"apply_patch"}, Kind: "custom", Forced: true},
		`{"type":"allowed_tools","mode":"auto","tools":[{"name":"b"},{"name":"a"},{"name":"b"}]}`: {Only: []string{"a", "b"}},
	}
	for raw, want := range tests {
		got := mustParseToolChoice(t, raw).Scope()
		if got.None != want.None || got.Kind != want.Kind || got.Forced != want.Forced || !slices.Equal(got.Only, want.Only) {
			t.Fatalf("ParseToolChoice(%s).Scope() = %#v, want %#v", raw, got, want)
		}
	}
}

// Warm-session reuse turns on scope equality, so it must compare the catalog a
// choice produces rather than how the client spelled it.
func TestToolScopeEqualComparesCatalogsNotSpellings(t *testing.T) {
	t.Parallel()
	auto := mustParseToolChoice(t, `"auto"`).Scope()
	if !auto.Equal(mustParseToolChoice(t, ``).Scope()) {
		t.Fatal("auto and an omitted tool_choice must name the same catalog")
	}
	if auto.Equal(mustParseToolChoice(t, `"none"`).Scope()) {
		t.Fatal("auto and none must not name the same catalog")
	}
	forced := mustParseToolChoice(t, `{"type":"function","name":"lookup"}`).Scope()
	allowed := mustParseToolChoice(t, `{"type":"allowed_tools","mode":"auto","tools":[{"name":"lookup"}]}`).Scope()
	if forced.Equal(allowed) {
		t.Fatal("a kind-specific forced choice must not reuse a name-only allowed_tools catalog")
	}
	custom := mustParseToolChoice(t, `{"type":"custom","name":"lookup"}`).Scope()
	if forced.Equal(custom) {
		t.Fatal("same-named forced choices of different kinds must not compare equal")
	}
	if forced.Equal(mustParseToolChoice(t, `{"type":"function","name":"other"}`).Scope()) {
		t.Fatal("forced choices for different tools must not compare equal")
	}
}

// Every one of these is a valid OpenAI request that a widely-used client sends
// by default, so validation must accept it.
func TestValidateChatAcceptsCommonClientParameters(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			req := decodeChatRequest(t, tt.body)
			assertChatAccepted(t, req)
		})
	}
}

func TestValidateChatRejectsInvalidMaxTokens(t *testing.T) {
	t.Parallel()
	for _, param := range []string{"max_tokens", "max_completion_tokens"} {
		assertChatRejected(t, decodeChatRequest(t, chatBody(param, `0`)), param)
		assertChatRejected(t, decodeChatRequest(t, chatBody(param, `-1`)), param)
	}
}

func TestValidateChatRejectsUnusableToolNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "has space", "dotted.name", strings.Repeat("a", 65)} {
		req := decodeChatRequest(t, `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"`+name+`"}}]}`)
		if err := ValidateChatRequest(req); err == nil {
			t.Fatalf("function tool name %q should be rejected", name)
		}
	}
}

func TestValidateResponsesAcceptsCommonClientParameters(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			assertResponsesAccepted(t, decodeResponsesRequest(t, tt.body))
		})
	}
}

func TestValidateResponsesRejectsInvalidMaxOutputTokens(t *testing.T) {
	t.Parallel()
	assertResponsesRejected(t, decodeResponsesRequest(t, responsesBody("max_output_tokens", `0`)), "max_output_tokens")
	assertResponsesRejected(t, decodeResponsesRequest(t, responsesBody("max_output_tokens", `"lots"`)), "max_output_tokens")
}

func TestChatAcceptsIgnoredSamplingFields(t *testing.T) {
	t.Parallel()
	var req ChatCompletionRequest
	body := []byte(`{"model":"gpt-5","temperature":0.1,"messages":[{"role":"user","content":"hi"}]}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateChatRequest(&req); err != nil {
		t.Fatalf("temperature should be accepted: %v", err)
	}
}

func TestChatRejectsUnsafeUnsupportedFields(t *testing.T) {
	t.Parallel()
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var req ChatCompletionRequest
			if err := json.Unmarshal([]byte(tt.body), &req); err != nil {
				t.Fatal(err)
			}
			err := ValidateChatRequest(&req)
			if err == nil {
				t.Fatal("expected unsafe field rejection")
			}
			apiErr, ok := err.(*apierr.Error)
			if !ok || apiErr.Param != tt.param {
				t.Fatalf("error = %#v, want param %q", err, tt.param)
			}
		})
	}
}

// logprobs and top_logprobs are the Chat Completions counterparts of the
// Responses include value "message.output_text.logprobs": all three ask for
// extra per-token detail alongside the reply, none of them change its shape.
// Ignoring them yields the same prose minus an optional annotation, which is
// exactly the graceful degradation the policy accepts, so both are taken.
func TestChatAcceptsAndIgnoresLogprobs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		body  string
		param string
	}{
		{
			name:  "logprobs true",
			body:  `{"model":"gpt-5","logprobs":true,"messages":[{"role":"user","content":"hi"}]}`,
			param: "logprobs",
		},
		{
			name:  "top_logprobs",
			body:  `{"model":"gpt-5","top_logprobs":5,"messages":[{"role":"user","content":"hi"}]}`,
			param: "top_logprobs",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := decodeChatRequest(t, tt.body)
			assertChatAccepted(t, req)
		})
	}
}

func TestChatAllowsSingleChoiceN(t *testing.T) {
	t.Parallel()
	var req ChatCompletionRequest
	body := []byte(`{"model":"gpt-5","n":1,"messages":[{"role":"user","content":"hi"}]}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateChatRequest(&req); err != nil {
		t.Fatalf("n=1 should be accepted: %v", err)
	}
}

func TestResponsesRejectsUnsafeUnsupportedFields(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
			var req ResponsesRequest
			if err := json.Unmarshal([]byte(tt.body), &req); err != nil {
				t.Fatal(err)
			}
			err := ValidateResponsesRequest(&req)
			if err == nil {
				t.Fatal("expected unsafe field rejection")
			}
			apiErr, ok := err.(*apierr.Error)
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
	t.Parallel()
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
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			chatBodyJSON := `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`
			responsesBodyJSON := `{"model":"gpt-5","input":"hi"}`
			if tt.format != "" {
				chatBodyJSON = chatBody("response_format", tt.format)
				responsesBodyJSON = responsesBody("text", `{"format":`+tt.format+`}`)
			}
			chat := decodeChatRequest(t, chatBodyJSON)
			responses := decodeResponsesRequest(t, responsesBodyJSON)
			if tt.chatParam == "" {
				assertChatAccepted(t, chat)
				assertResponsesAccepted(t, responses)
				return
			}
			assertChatRejected(t, chat, tt.chatParam)
			assertResponsesRejected(t, responses, tt.responsesParam)

			// The two surfaces must explain the refusal identically, otherwise a
			// client that switches surfaces learns two different things.
			chatMessage := ValidateChatRequest(chat).Error()
			responsesMessage := ValidateResponsesRequest(responses).Error()
			if got := strings.ReplaceAll(chatMessage, "response_format", "text.format"); got != responsesMessage {
				t.Fatalf("messages diverge:\n chat: %s\n responses: %s", chatMessage, responsesMessage)
			}
			if tt.chatParam == "response_format" && tt.format != `"json_schema"` && !strings.Contains(chatMessage, "structured output is not supported") {
				t.Fatalf("message = %q, want it to name the unsupported feature", chatMessage)
			}
		})
	}
}

func TestResponsesInputMayBeOmittedForPreviousResponseContinuation(t *testing.T) {
	t.Parallel()
	var req ResponsesRequest
	body := []byte(`{"model":"gpt-5","previous_response_id":"resp_previous"}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResponsesRequest(&req); err != nil {
		t.Fatalf("missing input should be accepted with previous_response_id: %v", err)
	}
}

func TestResponsesAllowsIgnoredFields(t *testing.T) {
	t.Parallel()
	var req ResponsesRequest
	body := []byte(`{"model":"gpt-5","temperature":0.1,"top_p":0.9,"input":"hi"}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResponsesRequest(&req); err != nil {
		t.Fatalf("ignored fields should be accepted: %v", err)
	}
}

func TestResponsesAcceptsCodexReasoningDefaultsAndClientOwnedTools(t *testing.T) {
	t.Parallel()
	var req ResponsesRequest
	body := []byte(`{"model":"gpt-5.5","include":["reasoning.encrypted_content"],"reasoning":{"effort":"medium","summary":"auto"},"text":{"verbosity":"low"},"tools":[{"type":"function","name":"exec_command","description":"run","parameters":{"type":"object","properties":{}}},{"type":"function","name":"multi_tool_use.parallel","description":"parallel","parameters":{"type":"object","properties":{}}},{"type":"custom","name":"apply_patch","description":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},{"type":"namespace","name":"mcp__grep_app","tools":[{"name":"searchGitHub","description":"search","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}]},{"type":"tool_search","execution":"client","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}],"input":"hi"}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResponsesRequest(&req); err != nil {
		t.Fatalf("Codex reasoning defaults and Responses tools should be accepted: %v", err)
	}
	if got := ResponsesReasoningEffort(&req); got != "medium" {
		t.Fatalf("ResponsesReasoningEffort = %q, want medium", got)
	}
	normalized, err := NormalizeResponsesTools(req.Tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 5 || normalized[2].Kind != toolcatalog.ToolKindCustom || normalized[3].Kind != toolcatalog.ToolKindNamespace || normalized[4].Kind != toolcatalog.ToolKindToolSearch {
		t.Fatalf("normalized tools = %#v, want function/function/custom/namespace/tool_search", normalized)
	}
}

func TestResponsesIgnoresHostedTools(t *testing.T) {
	t.Parallel()
	var req ResponsesRequest
	body := []byte(`{"model":"gpt-5.5","tools":[{"type":"web_search","external_web_access":true}],"input":"hi"}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResponsesRequest(&req); err != nil {
		t.Fatalf("hosted web_search should be ignored: %v", err)
	}
	normalized, err := NormalizeResponsesTools(req.Tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(normalized) != 0 {
		t.Fatalf("normalized hosted tools = %#v, want none", normalized)
	}
	if got := IgnoredResponsesToolTypes(req.Tools); len(got) != 1 || got[0] != "web_search" {
		t.Fatalf("IgnoredResponsesToolTypes = %#v, want [web_search] so the drop can be logged", got)
	}
}

// A tool type that is neither supported nor a known hosted tool is a client
// typo. Dropping it silently would ship a request with a missing capability, so
// it is a 400.
func TestResponsesRejectsUnknownToolType(t *testing.T) {
	t.Parallel()
	var req ResponsesRequest
	body := []byte(`{"model":"gpt-5.5","tools":[{"type":"funcion","name":"lookup"}],"input":"hi"}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	err := ValidateResponsesRequest(&req)
	apiErr, ok := err.(*apierr.Error)
	if !ok || apiErr.Param != "tools.0.type" {
		t.Fatalf("ValidateResponsesRequest() = %#v, want a 400 on tools.0.type", err)
	}
	if got := IgnoredResponsesToolTypes(req.Tools); len(got) != 0 {
		t.Fatalf("IgnoredResponsesToolTypes = %#v, want none for an unknown type", got)
	}
}

func TestValidateChatRejectsUnsupportedToolTypes(t *testing.T) {
	t.Parallel()
	var req ChatCompletionRequest
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","name":"apply_patch"}]}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateChatRequest(&req); err == nil {
		t.Fatal("expected Chat custom tools to be rejected")
	}
}

func TestNewResponseUsageUsesResponsesTokenNames(t *testing.T) {
	t.Parallel()
	reasoning := int64(2)
	usage := NewResponseUsage(&Usage{PromptTokens: 3, CompletionTokens: 5, CompletionTokensDetails: &TokenDetails{ReasoningTokens: &reasoning}})
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

// Every member of the Responses usage object is required, so an emitted object
// carries all of them even when the turn only reported one counter. Absence is
// expressible only by the nil parent.
func TestNewResponseUsageAlwaysEmitsRequiredMembers(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(NewResponseUsage(&Usage{CompletionTokens: 12, TotalTokens: 12}))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"input_tokens":0,"input_tokens_details":{"cache_write_tokens":0,"cached_tokens":0},"output_tokens":12,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":12}`; got != want {
		t.Fatalf("usage JSON = %s, want %s", got, want)
	}
	if usage := NewResponseUsage(nil); usage != nil {
		t.Fatalf("NewResponseUsage(nil) = %#v, want nil", usage)
	}
}

func TestContentTextRejectsImages(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	var req ChatCompletionRequest
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA","detail":"low"}}]}]}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateChatRequest(&req); err != nil {
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
	t.Parallel()
	var req ChatCompletionRequest
	body := []byte(`{"model":"gpt-5","messages":[{"role":"assistant","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateChatRequest(&req); err == nil {
		t.Fatal("expected assistant image content rejection")
	}
}

// reasoning effort is an enum this proxy acts on, so a value outside the enum is
// the reject case of the validation policy: accepting and ignoring it would run
// the turn at a different effort than the client asked for, with nothing on the
// wire to say so. Each surface has to name its own param.
func TestValidateRejectsUnknownReasoningEffort(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		path  string
		body  string
		param string
	}{
		{
			name:  "chat reasoning_effort",
			path:  "chat",
			body:  `{"model":"gpt-5","reasoning_effort":"banana","messages":[{"role":"user","content":"hi"}]}`,
			param: "reasoning_effort",
		},
		{
			name:  "responses reasoning_effort",
			path:  "responses",
			body:  `{"model":"gpt-5","reasoning_effort":"banana","input":"hi"}`,
			param: "reasoning_effort",
		},
		{
			name:  "responses reasoning.effort",
			path:  "responses",
			body:  `{"model":"gpt-5","reasoning":{"effort":"banana"},"input":"hi"}`,
			param: "reasoning.effort",
		},
		{
			name:  "responses reasoning.effort ignoring case and padding",
			path:  "responses",
			body:  `{"model":"gpt-5","reasoning":{"effort":" Banana "},"input":"hi"}`,
			param: "reasoning.effort",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := validateBodyForSurface(t, tt.path, tt.body)
			if err == nil {
				t.Fatal("expected unknown reasoning effort rejection")
			}
			apiErr, ok := err.(*apierr.Error)
			if !ok || apiErr.Kind != apierr.KindInvalidInput || apiErr.Param != tt.param {
				t.Fatalf("error = %#v, want invalid_request_error param %q", err, tt.param)
			}
		})
	}
}

func TestValidateAcceptsKnownReasoningEfforts(t *testing.T) {
	t.Parallel()
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", " HIGH "} {
		t.Run(effort, func(t *testing.T) {
			t.Parallel()
			chat := `{"model":"gpt-5","reasoning_effort":"` + effort + `","messages":[{"role":"user","content":"hi"}]}`
			if err := validateBodyForSurface(t, "chat", chat); err != nil {
				t.Fatalf("chat reasoning_effort %q: %v", effort, err)
			}
			responses := `{"model":"gpt-5","reasoning":{"effort":"` + effort + `"},"input":"hi"}`
			if err := validateBodyForSurface(t, "responses", responses); err != nil {
				t.Fatalf("responses reasoning.effort %q: %v", effort, err)
			}
		})
	}
}

func validateBodyForSurface(t *testing.T, surface, body string) error {
	t.Helper()
	if surface == "chat" {
		var req ChatCompletionRequest
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatal(err)
		}
		return ValidateChatRequest(&req)
	}
	var req ResponsesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return ValidateResponsesRequest(&req)
}
