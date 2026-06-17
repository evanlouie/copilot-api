package copilotgw

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	copilot "github.com/github/copilot-sdk/go"
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

func responseToolOutputs(values map[string]string) map[string]openai.ResponseToolOutput {
	out := make(map[string]openai.ResponseToolOutput, len(values))
	for id, value := range values {
		out[id] = openai.ResponseToolOutput{Kind: openai.ToolKindFunction, CallID: id, Output: value}
	}
	return out
}

func TestResponseContinuationBatchSelectsLiveSubsetFromCodexHistory(t *testing.T) {
	broker := toolproxy.NewBroker(time.Minute)
	rt, err := toolproxy.NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	oldBatch, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_old", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_old", "response", "gpt-test", make(chan toolproxy.TurnFinalResult, 1), nil)
	if err := oldBatch.Complete(map[string]string{"call_old": "old"}); err != nil {
		t.Fatal(err)
	}
	batch, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_current", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_current", "response", "gpt-test", make(chan toolproxy.TurnFinalResult, 1), nil)
	g := &RealGateway{broker: broker}
	found, active, err := g.responseContinuationBatch(responseToolOutputs(map[string]string{"call_old": "old", "call_missing": "missing", "call_current": "current"}))
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != batch.ID {
		t.Fatalf("found batch = %q, want %q", found.ID, batch.ID)
	}
	if len(active) != 1 || active["call_current"].Output != "current" {
		t.Fatalf("active outputs = %#v, want only current call", active)
	}
}

func TestResponseContinuationBatchKeepsAllLiveParallelOutputs(t *testing.T) {
	broker := toolproxy.NewBroker(time.Minute)
	rt, err := toolproxy.NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	oldBatch, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_old", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_old", "response", "gpt-test", make(chan toolproxy.TurnFinalResult, 1), nil)
	if err := oldBatch.Complete(map[string]string{"call_old": "old"}); err != nil {
		t.Fatal(err)
	}
	batch, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{
		{ToolCallID: "call_current_1", Name: rt.Tools()[0].Name, Arguments: map[string]any{"q": "one"}},
		{ToolCallID: "call_current_2", Name: rt.Tools()[0].Name, Arguments: map[string]any{"q": "two"}},
	}, "resp_current", "response", "gpt-test", make(chan toolproxy.TurnFinalResult, 1), nil)
	g := &RealGateway{broker: broker}
	found, active, err := g.responseContinuationBatch(responseToolOutputs(map[string]string{"call_old": "old", "call_current_1": "one", "call_current_2": "two"}))
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != batch.ID {
		t.Fatalf("found batch = %q, want %q", found.ID, batch.ID)
	}
	if len(active) != 2 || active["call_current_1"].Output != "one" || active["call_current_2"].Output != "two" {
		t.Fatalf("active outputs = %#v, want both current calls", active)
	}
}

func TestResponseContinuationBatchRejectsAmbiguousLiveBatches(t *testing.T) {
	broker := toolproxy.NewBroker(time.Minute)
	rt, err := toolproxy.NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	oldBatch, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_old", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_old", "response", "gpt-test", make(chan toolproxy.TurnFinalResult, 1), nil)
	if err := oldBatch.Complete(map[string]string{"call_old": "old"}); err != nil {
		t.Fatal(err)
	}
	_, _, _ = rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_live_1", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_live_1", "response", "gpt-test", make(chan toolproxy.TurnFinalResult, 1), nil)
	// Simulate a second independent live pending batch from another response.
	rt2, err := toolproxy.NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = rt2.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_live_2", Name: rt2.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_live_2", "response", "gpt-test", make(chan toolproxy.TurnFinalResult, 1), nil)
	g := &RealGateway{broker: broker}
	_, _, err = g.responseContinuationBatch(responseToolOutputs(map[string]string{"call_old": "old", "call_live_1": "one", "call_live_2": "two"}))
	if err == nil || errors.Is(err, toolproxy.ErrNotFound) || !strings.Contains(err.Error(), "different pending batches") {
		t.Fatalf("error = %v, want ambiguous live batch invalid request", err)
	}
}

func TestResponseFallbackWithoutPreviousResponseUsesTranscriptInput(t *testing.T) {
	g := &RealGateway{}
	fallback, err := g.responseFallbackRequestFromFunctionOutputs(ResponseRequest{
		Model:                              "gpt-test",
		Instructions:                       "continuation-only",
		ToolOutputs:                        responseToolOutputs(map[string]string{"call_old": "old"}),
		FunctionOutputFallbackAvailable:    true,
		FunctionOutputFallbackInput:        openai.PromptContent{Text: "User:\nlook up alpha\n\nFunction output call_old:\nold\n\nUser:\nnow summarize"},
		FunctionOutputFallbackInstructions: "base\n\nDeveloper:\ndesktop context",
	})
	if err != nil {
		t.Fatal(err)
	}
	if fallback.PreviousResponseID != "" || len(fallback.ToolOutputs) != 0 || fallback.FunctionOutputFallbackAvailable {
		t.Fatalf("fallback continuation fields not cleared: %#v", fallback)
	}
	if fallback.Input.Text == "" || !strings.Contains(fallback.Input.Text, "Function output call_old") || !strings.Contains(fallback.Input.Text, "now summarize") {
		t.Fatalf("fallback input = %q, want transcript input", fallback.Input.Text)
	}
	if fallback.Instructions != "base\n\nDeveloper:\ndesktop context" {
		t.Fatalf("fallback instructions = %q, want transcript instructions", fallback.Instructions)
	}
}

func TestStreamingResponseContinuationDefaultsMissingResponseID(t *testing.T) {
	store := sessionstore.New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	g := NewReal(config.Config{ToolCallTTL: time.Minute}, store, nil)
	g.modelsFetcher = func(context.Context) ([]Model, error) { return []Model{{ID: "gpt-test"}}, nil }

	previous := responseFromTurn("resp_prev", "gpt-test", "", nil, true, &TurnResult{Text: "need lookup", FinishReason: "tool_calls"}, false)
	if err := store.SaveResponse(recordFromResponse(previous, "sdk-session", "")); err != nil {
		t.Fatal(err)
	}
	rt, err := toolproxy.NewRequestTools(g.broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	batch, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_prev", "response", "gpt-test", make(chan toolproxy.TurnFinalResult, 1), nil)
	runner := &turnRunner{responseID: "resp_prev", updates: make(chan toolproxy.TurnFinalResult, 1), closed: make(chan struct{})}
	defer close(runner.closed)
	g.rememberRunner(batch.ID, runner)

	ch, err := g.StreamResponse(context.Background(), ResponseRequest{Model: "gpt-test", ToolOutputs: responseToolOutputs(map[string]string{"call_1": "ok"})})
	if err != nil {
		t.Fatal(err)
	}
	if ch == nil {
		t.Fatal("StreamResponse returned nil channel")
	}
	runner.updates <- toolproxy.TurnFinalResult{Value: &TurnResult{}}
	got := runner.currentResponseID()
	if got == "" || got == "resp_prev" || !strings.HasPrefix(got, "resp_") {
		t.Fatalf("streaming continuation response id = %q, want generated continuation id", got)
	}
}

func TestResponseFallbackWithPreviousResponseUsesExtendedToolLabels(t *testing.T) {
	store := sessionstore.New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	g := &RealGateway{store: store}
	previous := &openai.Response{
		ID:        "resp_prev",
		CreatedAt: openai.UnixNow(),
		Status:    "completed",
		Model:     "gpt-test",
		Store:     true,
		Output: []openai.ResponseOutputItem{
			{ID: "ctc_call_patch", Type: "custom_tool_call", Status: "completed", CallID: "call_patch", Name: "apply_patch", Input: "*** Begin Patch"},
			{ID: "tsc_call_search", Type: "tool_search_call", Status: "completed", CallID: "call_search", Execution: "client", ArgumentsJSON: json.RawMessage(`{"query":"grep"}`)},
			{ID: "fc_call_mcp", Type: "function_call", Status: "completed", CallID: "call_mcp", Namespace: "mcp__grep_app", Name: "searchGitHub", Arguments: `{"query":"repo:test"}`},
		},
	}
	if err := store.SaveResponse(recordFromResponse(previous, "sdk-session", "")); err != nil {
		t.Fatal(err)
	}
	fallback, err := g.responseFallbackRequestFromFunctionOutputs(ResponseRequest{
		Model:              "gpt-test",
		PreviousResponseID: "resp_prev",
		ToolOutputs: map[string]openai.ResponseToolOutput{
			"call_patch":  {Kind: openai.ToolKindCustom, CallID: "call_patch", Name: "apply_patch", Output: "patched"},
			"call_search": {Kind: openai.ToolKindToolSearch, CallID: "call_search", Execution: "client", Status: "completed", Output: "loaded", Tools: json.RawMessage(`[{"type":"function","name":"loaded_tool"}]`)},
			"call_mcp":    {Kind: openai.ToolKindFunction, CallID: "call_mcp", Output: "results"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := fallback.Input.Text
	for _, want := range []string{
		"Custom tool output call_patch for apply_patch",
		"Assistant call: custom_tool_call apply_patch input=*** Begin Patch",
		"Tool search output call_search (execution=client, status=completed)",
		"Assistant call: tool_search_call arguments={\"query\":\"grep\"}",
		"Returned tools: [{\"type\":\"function\",\"name\":\"loaded_tool\"}]",
		"Function output call_mcp for mcp__grep_app.searchGitHub",
		"Assistant call: function_call mcp__grep_app.searchGitHub arguments={\"query\":\"repo:test\"}",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("fallback prompt missing %q:\n%s", want, text)
		}
	}
}

func TestResponseFallbackWithoutPreviousResponseRejectsUnavailableTranscript(t *testing.T) {
	g := &RealGateway{}
	_, err := g.responseFallbackRequestFromFunctionOutputs(ResponseRequest{
		Model:       "gpt-test",
		ToolOutputs: responseToolOutputs(map[string]string{"call_orphan": "orphan result"}),
	})
	if err == nil || !strings.Contains(err.Error(), "previous_response_id is required") {
		t.Fatalf("error = %v, want previous_response_id requirement", err)
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
