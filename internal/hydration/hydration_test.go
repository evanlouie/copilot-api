package hydration

import (
	"encoding/json"
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
		var event copilot.SessionEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
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

func TestBuildChatHistoryReplaysInboundReasoning(t *testing.T) {
	assistant := openai.ChatMessage{Role: "assistant", Content: openai.NewTextContent("done")}
	// Simulate a client round-tripping our own reasoning output back to us.
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"done","reasoning":"I considered the options"}`), &assistant); err != nil {
		t.Fatal(err)
	}
	res, err := BuildChatHistory([]openai.ChatMessage{
		{Role: "user", Content: openai.NewTextContent("hi")},
		assistant,
	}, Options{SessionID: "synth-reasoning", Model: "claude-sonnet-4.6", Now: time.Unix(1, 0).UTC()})
	if err != nil {
		t.Fatalf("inbound reasoning must not error: %v", err)
	}
	found := false
	for _, ev := range res.Events {
		if msg, ok := ev.Data.(*copilot.AssistantMessageData); ok {
			if msg.ReasoningText == nil || *msg.ReasoningText != "I considered the options" {
				t.Fatalf("assistant reasoning not replayed: %#v", msg.ReasoningText)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("no assistant message event produced")
	}
	if !strings.Contains(string(res.JSONL), "I considered the options") {
		t.Fatalf("reasoning text missing from JSONL: %s", res.JSONL)
	}
}

func TestBuildChatHistoryPrefersReasoningOverReasoningContent(t *testing.T) {
	var assistant openai.ChatMessage
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"done","reasoning":"canonical","reasoning_content":"alias"}`), &assistant); err != nil {
		t.Fatal(err)
	}
	if got := assistant.InboundReasoning(); got != "canonical" {
		t.Fatalf("InboundReasoning = %q, want canonical", got)
	}
}

func TestBuildChatHistoryReplaysReasoningDetailsText(t *testing.T) {
	var assistant openai.ChatMessage
	// OpenRouter-style client that round-trips only reasoning_details.
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"done","reasoning_details":[{"type":"reasoning.text","text":"from details","signature":"sig"}]}`), &assistant); err != nil {
		t.Fatal(err)
	}
	if got := assistant.InboundReasoning(); got != "from details" {
		t.Fatalf("InboundReasoning = %q, want details fallback", got)
	}
	res, err := BuildChatHistory([]openai.ChatMessage{
		{Role: "user", Content: openai.NewTextContent("hi")},
		assistant,
	}, Options{SessionID: "synth-details", Model: "claude-sonnet-4.6", Now: time.Unix(1, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.JSONL), "from details") {
		t.Fatalf("reasoning_details text not replayed into JSONL: %s", res.JSONL)
	}
}

func TestBuildChatHistoryReplaysReasoningDetailsSummaryBlocks(t *testing.T) {
	var assistant openai.ChatMessage
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"done","reasoning_details":[{"type":"reasoning.summary","summary":"first "},{"type":"reasoning.text","text":"second"},{"type":"reasoning.encrypted","data":"enc"}]}`), &assistant); err != nil {
		t.Fatal(err)
	}
	if got := assistant.InboundReasoning(); got != "first second" {
		t.Fatalf("InboundReasoning = %q, want concatenated summary/text details", got)
	}
	res, err := BuildChatHistory([]openai.ChatMessage{
		{Role: "user", Content: openai.NewTextContent("hi")},
		assistant,
	}, Options{SessionID: "synth-summary-details", Model: "claude-sonnet-4.6", Now: time.Unix(1, 0).UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(res.JSONL), "first second") {
		t.Fatalf("reasoning_details summary/text not replayed into JSONL: %s", res.JSONL)
	}
	if strings.Contains(string(res.JSONL), "enc") {
		t.Fatalf("encrypted reasoning should not be replayed into cold JSONL: %s", res.JSONL)
	}
}

func TestBuildChatHistoryMessagesIncludesUserAttachments(t *testing.T) {
	data := "AAAA"
	mimeType := "image/png"
	displayName := "image.png"
	res, err := BuildChatHistoryMessages([]Message{{
		Role:    "user",
		Content: "describe",
		Attachments: []copilot.Attachment{copilot.AttachmentBlob{
			Data:        data,
			MIMEType:    mimeType,
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
		var event copilot.SessionEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("event did not round-trip: %v\n%s", err, line)
		}
	}
}
