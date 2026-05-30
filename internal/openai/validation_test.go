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

func TestStrictChatRejectsSilentlyIgnoredSamplingFields(t *testing.T) {
	var req ChatCompletionRequest
	body := []byte(`{"model":"gpt-5","temperature":0.1,"messages":[{"role":"user","content":"hi"}]}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	if err := ValidateChatRequest(&req, true); err == nil {
		t.Fatal("expected temperature rejection in strict mode")
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
}
