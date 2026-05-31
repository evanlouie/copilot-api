package openai

import (
	"encoding/json"
	"testing"
)

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
			name:  "text",
			body:  `{"model":"gpt-5","text":{"format":{"type":"json_object"}},"input":"hi"}`,
			param: "text",
		},
		{
			name:  "reasoning",
			body:  `{"model":"gpt-5","reasoning":{"effort":"high"},"input":"hi"}`,
			param: "reasoning",
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
