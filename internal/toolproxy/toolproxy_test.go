package toolproxy

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

func TestRequestToolsNoneUsesSentinel(t *testing.T) {
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := rt.AvailableTools(); len(got) != 1 || got[0] != NoToolsSentinel {
		t.Fatalf("unexpected available tools: %#v", got)
	}
}

func TestRequestToolsUnsupportedOnlyUsesSentinel(t *testing.T) {
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "custom"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Tools()) != 0 {
		t.Fatalf("SDK tools = %#v, want none", rt.Tools())
	}
	if got := rt.AvailableTools(); len(got) != 1 || got[0] != NoToolsSentinel {
		t.Fatalf("unexpected available tools: %#v", got)
	}
}

func TestRequestToolsExposePublicNamesAsCustomFilters(t *testing.T) {
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{
		{Type: "function", Function: openai.FunctionTool{Name: "get-weather"}},
		{Type: "function", Function: openai.FunctionTool{Name: "grep"}},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Tools()) != 2 {
		t.Fatalf("SDK tools = %#v, want two", rt.Tools())
	}
	gotNames := []string{rt.Tools()[0].Name, rt.Tools()[1].Name}
	wantNames := []string{"get-weather", "grep"}
	if !reflect.DeepEqual(gotNames, wantNames) {
		t.Fatalf("tool names = %#v, want %#v", gotNames, wantNames)
	}
	for _, tool := range rt.Tools() {
		if strings.HasPrefix(tool.Name, "capi_") {
			t.Fatalf("tool name %q still uses capi_ alias", tool.Name)
		}
		if !tool.OverridesBuiltInTool {
			t.Fatalf("tool %q should opt into built-in override", tool.Name)
		}
	}
	if got, want := rt.AvailableTools(), []string{"custom:get-weather", "custom:grep"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("available tools = %#v, want %#v", got, want)
	}
}

func TestCaptureRequestsUsesPublicToolName(t *testing.T) {
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "get-weather"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	batch, calls := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: "get-weather", Arguments: map[string]any{"city": "Paris"}}}, "", "chat", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if len(calls) != 1 {
		t.Fatalf("calls = %#v, want one", calls)
	}
	if got := calls[0].Function.Name; got != "get-weather" {
		t.Fatalf("tool call name = %q, want public name", got)
	}
	if got := batch.Calls["call_1"].PublicName; got != "get-weather" {
		t.Fatalf("batch public name = %q, want get-weather", got)
	}
}

func TestPermissionHandlerAllowsOnlyConfiguredCustomTools(t *testing.T) {
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	handler := rt.PermissionHandler()
	allowed, err := handler(copilot.PermissionRequestCustomTool{ToolName: rt.Tools()[0].Name}, copilot.PermissionInvocation{})
	if err != nil {
		t.Fatal(err)
	}
	if allowed.Kind() != rpc.PermissionDecisionKindApproveOnce {
		t.Fatalf("expected approve-once, got %s", allowed.Kind())
	}
	denied, err := handler(copilot.PermissionRequestCustomTool{ToolName: NoToolsSentinel}, copilot.PermissionInvocation{})
	if err != nil {
		t.Fatal(err)
	}
	if denied.Kind() != rpc.PermissionDecisionKindReject {
		t.Fatalf("expected reject, got %s", denied.Kind())
	}
	unknown, err := handler(copilot.PermissionRequestCustomTool{ToolName: "unknown_tool"}, copilot.PermissionInvocation{})
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Kind() != rpc.PermissionDecisionKindReject {
		t.Fatalf("expected reject for unknown tool, got %s", unknown.Kind())
	}
}

func TestCompletedBatchDoesNotCaptureNextInvocation(t *testing.T) {
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	batch, _ := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "", "chat", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err := batch.Complete(map[string]string{"call_1": "ok"}); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = rt.Tools()[0].Handler(copilot.ToolInvocation{ToolCallID: "call_2", ToolName: rt.Tools()[0].Name, Arguments: map[string]any{}})
	}()
	var next *Batch
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		next, err = broker.FindByCallIDs([]string{"call_2"})
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == batch.ID {
		t.Fatal("new invocation was attached to completed batch")
	}
	if err := next.Complete(map[string]string{"call_2": "ok"}); err != nil {
		t.Fatal(err)
	}
}

func TestFindByAnyCallIDsIgnoresStaleHistoryIDs(t *testing.T) {
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	oldBatch, _ := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_old", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_old", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err := oldBatch.Complete(map[string]string{"call_old": "old"}); err != nil {
		t.Fatal(err)
	}
	batch, _ := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_current", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_current", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	found, matched, err := broker.FindByAnyCallIDs([]string{"call_old", "call_missing", "call_current"})
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != batch.ID {
		t.Fatalf("found batch = %q, want %q", found.ID, batch.ID)
	}
	if len(matched) != 1 || matched[0] != "call_current" {
		t.Fatalf("matched = %#v, want only current call", matched)
	}
}

func TestFindByAnyCallIDsReturnsAllMatchedLiveIDs(t *testing.T) {
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	oldBatch, _ := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_old", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_old", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err := oldBatch.Complete(map[string]string{"call_old": "old"}); err != nil {
		t.Fatal(err)
	}
	batch, _ := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{
		{ToolCallID: "call_current_1", Name: rt.Tools()[0].Name, Arguments: map[string]any{}},
		{ToolCallID: "call_current_2", Name: rt.Tools()[0].Name, Arguments: map[string]any{}},
	}, "resp_current", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	found, matched, err := broker.FindByAnyCallIDs([]string{"call_old", "call_current_1", "call_current_2"})
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != batch.ID {
		t.Fatalf("found batch = %q, want %q", found.ID, batch.ID)
	}
	if len(matched) != 2 || matched[0] != "call_current_1" || matched[1] != "call_current_2" {
		t.Fatalf("matched = %#v, want both current calls", matched)
	}
}

func TestFindByAnyCallIDsRejectsMultipleLiveBatches(t *testing.T) {
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_1", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	rt2, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = rt2.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_2", Name: rt2.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_2", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	_, _, err = broker.FindByAnyCallIDs([]string{"call_1", "call_2"})
	if err == nil || !strings.Contains(err.Error(), "different pending batches") {
		t.Fatalf("error = %v, want different pending batches", err)
	}
}

func TestExpiredBatchIsRemovedFromBroker(t *testing.T) {
	broker := NewBroker(10 * time.Millisecond)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	batch, _ := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "", "chat", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if batch.Model != "gpt-test" {
		t.Fatalf("batch model = %q", batch.Model)
	}
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if _, err := broker.FindByCallIDs([]string{"call_1"}); err == ErrNotFound {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expired batch remained registered")
}

func TestBatchContextCancellationUnblocksHandler(t *testing.T) {
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt.SetContext(ctx)
	done := make(chan string, 1)
	go func() {
		_, err := rt.Tools()[0].Handler(copilot.ToolInvocation{ToolCallID: "call_cancel", ToolName: rt.Tools()[0].Name, Arguments: map[string]any{}})
		if err != nil {
			done <- err.Error()
			return
		}
		done <- "ok"
	}()
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		if _, err := broker.FindByCallIDs([]string{"call_cancel"}); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case got := <-done:
		if !strings.Contains(got, "canceled") {
			t.Fatalf("handler error = %q, want cancellation", got)
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not unblock after context cancellation")
	}
}

func TestBatchCompleteUnblocksHandler(t *testing.T) {
	broker := NewBroker(time.Minute)
	params := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup", Parameters: params}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.Tools()) != 1 {
		t.Fatal("expected one SDK tool")
	}
	done := make(chan string, 1)
	go func() {
		res, err := rt.Tools()[0].Handler(copilot.ToolInvocation{ToolCallID: "call_1", ToolName: rt.Tools()[0].Name, Arguments: map[string]any{"x": "y"}})
		if err != nil {
			done <- "ERR:" + err.Error()
			return
		}
		done <- res.TextResultForLLM
	}()
	var batch *Batch
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		batch, err = broker.FindByCallIDs([]string{"call_1"})
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.Complete(map[string]string{"call_1": "ok"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got != "ok" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("handler did not unblock")
	}
}

// TestParallelToolCallsRoundTripThroughBatch proves that two concurrent tool
// invocations are captured in a single batch and that one Complete call unblocks
// both handlers with their respective outputs — the core of parallel tool-call
// support.
func TestParallelToolCallsRoundTripThroughBatch(t *testing.T) {
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	tool := rt.Tools()[0]

	type result struct {
		out string
		err error
	}
	results := make(map[string]chan result, 2)
	for _, id := range []string{"call_1", "call_2"} {
		ch := make(chan result, 1)
		results[id] = ch
		callID := id
		go func() {
			res, err := tool.Handler(copilot.ToolInvocation{ToolCallID: callID, ToolName: tool.Name, Arguments: map[string]any{}})
			ch <- result{out: res.TextResultForLLM, err: err}
		}()
	}

	var batch *Batch
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		batch, err = broker.FindByCallIDs([]string{"call_1", "call_2"})
		if err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("both concurrent calls were not grouped into one batch: %v", err)
	}

	if err := batch.Complete(map[string]string{"call_1": "out-1", "call_2": "out-2"}); err != nil {
		t.Fatalf("Complete with both outputs failed: %v", err)
	}

	want := map[string]string{"call_1": "out-1", "call_2": "out-2"}
	for id, ch := range results {
		select {
		case got := <-ch:
			if got.err != nil {
				t.Fatalf("%s handler error: %v", id, got.err)
			}
			if got.out != want[id] {
				t.Fatalf("%s output = %q, want %q", id, got.out, want[id])
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s handler did not unblock", id)
		}
	}
}
