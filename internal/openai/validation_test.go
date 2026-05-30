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
