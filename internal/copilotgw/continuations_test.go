package copilotgw

import (
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/openai"
)

func TestFunctionOutputsWithContinuationInputAppendsFollowupDeterministically(t *testing.T) {
	outputs := map[string]string{"call_b": `{"ok":true}`, "call_a": "alpha"}
	out, err := functionOutputsWithContinuationInput(outputs, openai.PromptContent{Text: "Now optimize it."})
	if err != nil {
		t.Fatal(err)
	}
	if out["call_b"] != outputs["call_b"] {
		t.Fatalf("call_b output changed: %q", out["call_b"])
	}
	if !strings.Contains(out["call_a"], "alpha") || !strings.Contains(out["call_a"], "Additional user input after tool output:\nNow optimize it.") {
		t.Fatalf("call_a output = %q, want original output plus follow-up input", out["call_a"])
	}
	if outputs["call_a"] != "alpha" {
		t.Fatalf("original outputs mutated: %#v", outputs)
	}
}

func TestFunctionOutputsWithContinuationInputRejectsImages(t *testing.T) {
	_, err := functionOutputsWithContinuationInput(map[string]string{"call_1": "ok"}, openai.PromptContent{Images: []openai.ImageInput{{URL: "data:image/png;base64,AAAA"}}})
	if err == nil {
		t.Fatal("expected image follow-up rejection")
	}
}

func TestChatRequestFromContinuationDoesNotDuplicateToolOutputs(t *testing.T) {
	req := ChatContinuationRequest{
		Model: "gpt-5",
		Messages: []openai.ChatMessage{
			{Role: "user", Content: openai.NewTextContent("look up alpha")},
			{Role: "assistant", ToolCalls: []openai.ChatToolCall{{ID: "call_1", Type: "function", Function: openai.ToolCallFunction{Name: "lookup", Arguments: "{}"}}}},
			{Role: "tool", ToolCallID: "call_1", Content: openai.NewTextContent("alpha-result")},
		},
		Outputs: map[string]string{"call_1": "alpha-result"},
	}
	chatReq, err := chatRequestFromContinuation(req)
	if err != nil {
		t.Fatal(err)
	}
	// The whole transcript (including the tool result) is replayed via hydration,
	// so the synthetic prompt must not restate the outputs.
	if len(chatReq.History) != len(req.Messages) {
		t.Fatalf("History = %d messages, want full transcript of %d", len(chatReq.History), len(req.Messages))
	}
	text, err := chatReq.FinalUser.Text()
	if err != nil {
		t.Fatal(err)
	}
	if text != "Continue." {
		t.Fatalf("FinalUser = %q, want a minimal continuation prompt", text)
	}
	if strings.Contains(text, "alpha-result") {
		t.Fatalf("continuation prompt duplicated tool outputs already present in history: %q", text)
	}
}
