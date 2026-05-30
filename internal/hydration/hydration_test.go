package hydration

import (
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"

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

func TestBuildChatHistoryMessagesIncludesUserAttachments(t *testing.T) {
	data := "AAAA"
	mimeType := "image/png"
	displayName := "image.png"
	res, err := BuildChatHistoryMessages([]Message{{
		Role:    "user",
		Content: "describe",
		Attachments: []copilot.Attachment{{
			Type:        copilot.AttachmentTypeBlob,
			Data:        &data,
			MIMEType:    &mimeType,
			DisplayName: &displayName,
		}},
	}}, Options{SessionID: "synth-image", Model: "gpt-5", Now: time.Unix(1, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	jsonl := string(res.JSONL)
	for _, want := range []string{`"attachments"`, `"type":"blob"`, `"mimeType":"image/png"`, `"displayName":"image.png"`} {
		if !strings.Contains(jsonl, want) {
			t.Fatalf("expected %s in JSONL: %s", want, jsonl)
		}
	}
	for _, line := range strings.Split(strings.TrimSpace(jsonl), "\n") {
		if _, err := copilot.UnmarshalSessionEvent([]byte(line)); err != nil {
			t.Fatalf("event did not round-trip: %v\n%s", err, line)
		}
	}
}
