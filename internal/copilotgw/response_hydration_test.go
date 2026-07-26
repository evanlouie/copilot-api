package copilotgw

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/hydration"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionfs"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"

	copilot "github.com/github/copilot-sdk/go"
)

func newHydrationGateway(t *testing.T) *RealGateway {
	t.Helper()
	dataDir := t.TempDir()
	store := sessionstore.New(dataDir, t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	return &RealGateway{cfg: config.Config{DataDir: dataDir}, store: store, fs: sessionfs.NewManager(dataDir)}
}

func readHydratedEvents(t *testing.T, g *RealGateway, sessionID string) []copilot.SessionEvent {
	t.Helper()
	path := filepath.Join(g.cfg.DataDir, "sessions", sessionID, strings.TrimPrefix(sessionfs.SessionStatePath, "/"), "events.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no synthetic session events written: %v", err)
	}
	var events []copilot.SessionEvent
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var event copilot.SessionEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("event did not round-trip: %v\n%s", err, line)
		}
		events = append(events, event)
	}
	return events
}

// TestResponseColdContinuationReplaysChainAsSessionEvents pins the behavior this
// refactor introduced: a cold Responses continuation rebuilds the conversation
// as real Copilot session events (the mechanism Chat Completions already used)
// rather than flattening it into a prose blob inside one user prompt.
func TestResponseColdContinuationReplaysChainAsSessionEvents(t *testing.T) {
	g := newHydrationGateway(t)
	first := sessionstore.ResponseRecord{
		ID:        "resp_a",
		InputText: "remember alpha",
		Output: []openai.ResponseOutputItem{
			{Type: "reasoning", Summary: []openai.ResponseReasoningSummary{{Type: "summary_text", Text: "consider alpha"}}},
			{Type: "function_call", CallID: "call_1", Name: "lookup", Arguments: `{"q":"alpha"}`},
		},
		OutputText: "looking it up",
	}
	if err := g.store.SaveResponse(first); err != nil {
		t.Fatal(err)
	}
	second := sessionstore.ResponseRecord{
		ID:                 "resp_b",
		PreviousResponseID: "resp_a",
		ToolOutputs:        []toolcatalog.StoredToolOutput{{Type: "function_call_output", CallID: "call_1", Output: "alpha-result"}},
		OutputText:         "alpha is 1",
	}
	if err := g.store.SaveResponse(second); err != nil {
		t.Fatal(err)
	}

	if err := g.hydrateResponseContinuation("resp_sdk_cold", "gpt-5", second); err != nil {
		t.Fatalf("hydrateResponseContinuation: %v", err)
	}

	events := readHydratedEvents(t, g, "resp_sdk_cold")
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, string(event.Type()))
	}
	for _, want := range []copilot.SessionEventType{
		copilot.SessionEventTypeSessionStart,
		copilot.SessionEventTypeUserMessage,
		copilot.SessionEventTypeAssistantMessage,
		copilot.SessionEventTypeToolExecutionStart,
		copilot.SessionEventTypeToolExecutionComplete,
	} {
		if !slices.Contains(types, string(want)) {
			t.Fatalf("hydrated events %v missing %q", types, want)
		}
	}

	var user *copilot.UserMessageData
	var assistant *copilot.AssistantMessageData
	var toolDone *copilot.ToolExecutionCompleteData
	for _, event := range events {
		switch data := event.Data.(type) {
		case *copilot.UserMessageData:
			if user == nil {
				user = data
			}
		case *copilot.AssistantMessageData:
			if assistant == nil {
				assistant = data
			}
		case *copilot.ToolExecutionCompleteData:
			toolDone = data
		}
	}
	if user == nil || user.Content != "remember alpha" {
		t.Fatalf("first user message = %#v, want the recorded request text", user)
	}
	if assistant == nil || assistant.Content != "looking it up" {
		t.Fatalf("assistant message = %#v, want the recorded output text", assistant)
	}
	if assistant.ReasoningText == nil || *assistant.ReasoningText != "consider alpha" {
		t.Fatalf("assistant reasoning = %#v, want the recorded reasoning summary", assistant.ReasoningText)
	}
	if len(assistant.ToolRequests) != 1 || assistant.ToolRequests[0].ToolCallID != "call_1" || assistant.ToolRequests[0].Name != "lookup" {
		t.Fatalf("assistant tool requests = %#v, want the recorded function_call", assistant.ToolRequests)
	}
	if toolDone == nil || toolDone.Result == nil || toolDone.Result.Content != "alpha-result" {
		t.Fatalf("tool completion = %#v, want the recorded function_call_output", toolDone)
	}

	// The prose transcript this replaced must not appear anywhere in the
	// hydrated session.
	for _, event := range events {
		line, err := event.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(line), "Conversation so far from previous_response_id context") {
			t.Fatalf("hydrated session still carries the prose transcript: %s", line)
		}
	}
}

// TestResponseContinuationHistoryDegradesUnreplayableItems covers the Responses
// vocabulary that has no session-event equivalent. Those items must survive as
// text instead of becoming SDK tool requests that nothing ever answers.
func TestResponseContinuationHistoryDegradesUnreplayableItems(t *testing.T) {
	records := []sessionstore.ResponseRecord{
		{
			ID:        "resp_prev",
			InputText: "find tools",
			Output: []openai.ResponseOutputItem{
				{Type: "tool_search_call", CallID: "call_search", Execution: "client", ArgumentsJSON: json.RawMessage(`{"query":"agents"}`)},
				{Type: "function_call", CallID: "call_unanswered", Name: "lookup", Arguments: `{}`},
				{Type: "function_call", CallID: "call_bad_args", Name: "lookup", Arguments: `{"a":1} {"b":2}`},
			},
		},
		{
			ID:                 "resp_next",
			PreviousResponseID: "resp_prev",
			ToolOutputs: []toolcatalog.StoredToolOutput{
				{Type: "tool_search_output", CallID: "call_search", Execution: "client", Status: "completed", Output: "loaded"},
				{Type: "function_call_output", CallID: "call_bad_args", Output: "ignored"},
			},
			LoadedToolEvents: []toolcatalog.StoredLoadedToolEvent{
				{SourceCallID: "call_search", LoadedTools: []toolcatalog.StoredToolSpec{{Type: toolcatalog.ToolKindFunction, Name: "spawn_agent"}}},
			},
		},
	}
	messages := responseContinuationHistory(records)
	for _, msg := range messages {
		if msg.Role == "tool" {
			t.Fatalf("unreplayable tool output became a session tool result: %#v", msg)
		}
		for _, call := range msg.ToolCalls {
			t.Fatalf("unreplayable call became a session tool request: %#v", call)
		}
	}
	joined := ""
	for _, msg := range messages {
		joined += msg.Role + ": " + msg.Content + "\n"
	}
	for _, want := range []string{
		"tool_search_call arguments={\"query\":\"agents\"} call_id=call_search",
		"call_id=call_unanswered",
		"call_id=call_bad_args",
		"Tool search output call_search (execution=client, status=completed):",
		"Loaded tools from tool search call_search: spawn_agent",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("hydration history missing %q:\n%s", want, joined)
		}
	}
	// Hydration must still accept the degraded transcript.
	if _, err := hydration.BuildChatHistoryJSONL(messages, hydration.Options{Model: "gpt-test"}); err != nil {
		t.Fatalf("degraded history is not hydratable: %v", err)
	}
}

func TestResponseContinuationFollowUpKeepsClientInput(t *testing.T) {
	if got := responseContinuationFollowUp(resolvedPrompt{Text: "next question"}); got.Text != "next question" {
		t.Fatalf("prompt = %q, want the client's own input", got.Text)
	}
	if got := responseContinuationFollowUp(resolvedPrompt{Text: "  "}); got.Text != "Continue." {
		t.Fatalf("empty prompt = %q, want a minimal continuation nudge", got.Text)
	}
}
