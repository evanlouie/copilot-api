package hydration

import (
	"strings"
	"testing"
	"time"

	"copilot-api/internal/openai"

	copilot "github.com/github/copilot-sdk/go"
)

func TestBuildChatHistoryTextAndToolEventsRoundTrip(t *testing.T) {
	msgs := []openai.ChatMessage{
		{Role: "user", Content: openai.NewTextContent("remember alpha")},
		{Role: "assistant", Content: openai.NewTextContent("calling"), ToolCalls: []openai.ChatToolCall{{ID: "call_1", Type: "function", Function: openai.ToolCallFunction{Name: "lookup", Arguments: `{"q":"alpha"}`}}}},
		{Role: "tool", ToolCallID: "call_1", Content: openai.NewTextContent("tool-alpha")},
	}
	res, err := BuildChatHistory(msgs, Options{SessionID: "synth-test", Model: "gpt-5", Now: time.Unix(1, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Events) != 7 {
		t.Fatalf("expected 7 events, got %d", len(res.Events))
	}
	if !strings.Contains(string(res.JSONL), "tool.execution_complete") {
		t.Fatalf("expected tool execution event in JSONL: %s", res.JSONL)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(res.JSONL)), "\n") {
		if _, err := copilot.UnmarshalSessionEvent([]byte(line)); err != nil {
			t.Fatalf("event did not round-trip: %v\n%s", err, line)
		}
	}
}

func TestBuildChatHistoryRejectsUnknownToolResult(t *testing.T) {
	_, err := BuildChatHistory([]openai.ChatMessage{{Role: "tool", ToolCallID: "missing", Content: openai.NewTextContent("x")}}, Options{})
	if err == nil {
		t.Fatal("expected unknown tool result rejection")
	}
}
