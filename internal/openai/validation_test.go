package openai

import (
	"encoding/json"
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

func TestFoldChatInstructionsRejectsMidConversation(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", Content: NewTextContent("sys")},
		{Role: "user", Content: NewTextContent("hi")},
		{Role: "developer", Content: NewTextContent("late")},
	}
	_, _, err := FoldChatInstructions(msgs)
	if err == nil {
		t.Fatal("expected mid-conversation developer message to be rejected")
	}
}

func TestValidateToolChoice(t *testing.T) {
	var req ChatCompletionRequest
	body := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tool_choice":"required"}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateChatRequest(&req, true); err == nil {
		t.Fatal("expected required tool_choice rejection")
	}

	body = []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tool_choice":"none"}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateChatRequest(&req, true); err != nil {
		t.Fatalf("tool_choice none should be accepted: %v", err)
	}
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
			name:  "max tokens",
			body:  `{"model":"gpt-5","max_tokens":20,"messages":[{"role":"user","content":"hi"}]}`,
			param: "max_tokens",
		},
		{
			name:  "n greater than one",
			body:  `{"model":"gpt-5","n":2,"messages":[{"role":"user","content":"hi"}]}`,
			param: "n",
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
			name:  "text format",
			body:  `{"model":"gpt-5","text":{"format":{"type":"json_object"}},"input":"hi"}`,
			param: "text.format",
		},
		{
			name:  "unknown reasoning field",
			body:  `{"model":"gpt-5","reasoning":{"foo":"bar"},"input":"hi"}`,
			param: "reasoning.foo",
		},
		{
			name:  "unsupported include value",
			body:  `{"model":"gpt-5","include":["file_search_call.results"],"input":"hi"}`,
			param: "include",
		},
		{
			name:  "max output tokens",
			body:  `{"model":"gpt-5","max_output_tokens":20,"input":"hi"}`,
			param: "max_output_tokens",
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

func TestResponsesPermissiveAllowsCodexReasoningDefaults(t *testing.T) {
	var req ResponsesRequest
	body := []byte(`{"model":"gpt-5.5","include":["reasoning.encrypted_content"],"reasoning":{"effort":"medium","summary":"auto"},"text":{"verbosity":"low"},"tools":[{"type":"function","name":"exec_command","description":"run","parameters":{"type":"object","properties":{}}},{"type":"custom","name":"apply_patch","description":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},{"type":"web_search","external_web_access":true}],"input":"hi"}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateResponsesRequest(&req, false); err != nil {
		t.Fatalf("Codex reasoning defaults should be accepted in permissive mode: %v", err)
	}
	if got := ResponsesReasoningEffort(&req); got != "medium" {
		t.Fatalf("ResponsesReasoningEffort = %q, want medium", got)
	}
	supported := SupportedTools(req.Tools)
	if len(supported) != 1 || supported[0].Function.Name != "exec_command" {
		t.Fatalf("SupportedTools = %#v, want exec_command only", supported)
	}
	if err := ValidateResponsesRequest(&req, true); err == nil {
		t.Fatal("expected Codex-only ignored fields to be rejected in strict mode")
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
