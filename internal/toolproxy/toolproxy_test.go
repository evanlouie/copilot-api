package toolproxy

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

func TestRequestToolsNoneUsesSentinel(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, nil, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rt.AvailableTools(); len(got) != 1 || got[0] != NoToolsSentinel {
		t.Fatalf("unexpected available tools: %#v", got)
	}
}

func TestRequestToolsUnsupportedOnlyUsesSentinel(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "custom"}}, openai.ToolScope{})
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

// Chat Completions gets the same catalog narrowing as Responses; a forced
// function choice on that surface is enforced as far as "the model can call
// nothing else" reaches.
func TestChatForcedToolChoiceNarrowsTheCatalog(t *testing.T) {
	t.Parallel()
	tools := []openai.Tool{
		{Type: "function", Function: openai.FunctionTool{Name: "lookup"}},
		{Type: "function", Function: openai.FunctionTool{Name: "get_weather"}},
	}
	rt, err := NewRequestTools(NewBroker(time.Minute), tools, openai.ToolScope{Only: []string{"get_weather"}, Forced: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rt.AvailableTools(), []string{"custom:get_weather"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("available tools = %#v, want %#v", got, want)
	}
	// The withheld tool must also stop being callable, not merely stop being
	// advertised.
	if _, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_bad", Name: "lookup", Arguments: map[string]any{}}}, "chatcmpl_1", "chat", "gpt-test", make(chan TurnFinalResult, 1), nil); err == nil || !strings.Contains(err.Error(), "unconfigured SDK tool request") {
		t.Fatalf("error = %v, want the narrowed-away tool to be unconfigured", err)
	}
}

func TestRequestToolsExposePublicNamesAsCustomFilters(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{
		{Type: "function", Function: openai.FunctionTool{Name: "get-weather"}},
		{Type: "function", Function: openai.FunctionTool{Name: "grep"}},
	}, openai.ToolScope{})
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
	t.Parallel()
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "get-weather"}}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	batch, calls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: "get-weather", Arguments: map[string]any{"city": "Paris"}}}, "", "chat", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %#v, want one", calls)
	}
	if got := calls[0].ResponseName; got != "get-weather" {
		t.Fatalf("tool call name = %q, want public name", got)
	}
	captured, ok := batch.CapturedCall(calls[0].CallID)
	if !ok {
		t.Fatalf("batch is missing published call %q", calls[0].CallID)
	}
	if got := captured.ResponseName; got != "get-weather" {
		t.Fatalf("batch public name = %q, want get-weather", got)
	}
}

func TestPermissionHandlerAllowsOnlyConfiguredCustomTools(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, openai.ToolScope{})
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
	t.Parallel()
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	batch, calls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "", "chat", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.Complete(map[string]string{calls[0].CallID: "ok"}); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = rt.Tools()[0].Handler(copilot.ToolInvocation{ToolCallID: "call_2", ToolName: rt.Tools()[0].Name, Arguments: map[string]any{}})
	}()
	next, nextID := waitForSDKCall(t, broker, "call_2")
	if next.ID == batch.ID {
		t.Fatal("new invocation was attached to completed batch")
	}
	if found, err := broker.FindByCallIDs([]string{nextID}); err != nil || found.ID != next.ID {
		t.Fatalf("broker lookup of %q returned (%v, %v), want the new batch", nextID, found, err)
	}
	if err := next.Complete(map[string]string{nextID: "ok"}); err != nil {
		t.Fatal(err)
	}
}

func TestFindByAnyCallIDsIgnoresStaleHistoryIDs(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	oldBatch, oldCalls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_old", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_old", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	oldID := oldCalls[0].CallID
	if err := oldBatch.Complete(map[string]string{oldID: "old"}); err != nil {
		t.Fatal(err)
	}
	batch, currentCalls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_current", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_current", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	currentID := currentCalls[0].CallID
	found, matched, err := broker.FindByAnyCallIDs([]string{oldID, "call_missing", currentID})
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != batch.ID {
		t.Fatalf("found batch = %q, want %q", found.ID, batch.ID)
	}
	if len(matched) != 1 || matched[0] != currentID {
		t.Fatalf("matched = %#v, want only current call", matched)
	}
}

func TestFindByAnyCallIDsReturnsAllMatchedLiveIDs(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	oldBatch, oldCalls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_old", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_old", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	oldID := oldCalls[0].CallID
	if err := oldBatch.Complete(map[string]string{oldID: "old"}); err != nil {
		t.Fatal(err)
	}
	batch, currentCalls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{
		{ToolCallID: "call_current_1", Name: rt.Tools()[0].Name, Arguments: map[string]any{}},
		{ToolCallID: "call_current_2", Name: rt.Tools()[0].Name, Arguments: map[string]any{}},
	}, "resp_current", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	first, second := currentCalls[0].CallID, currentCalls[1].CallID
	found, matched, err := broker.FindByAnyCallIDs([]string{oldID, first, second})
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != batch.ID {
		t.Fatalf("found batch = %q, want %q", found.ID, batch.ID)
	}
	if len(matched) != 2 || matched[0] != first || matched[1] != second {
		t.Fatalf("matched = %#v, want both current calls", matched)
	}
}

func TestFindByAnyCallIDsRejectsMultipleLiveBatches(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	_, firstCalls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_1", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	rt2, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	_, secondCalls, err := rt2.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_2", Name: rt2.Tools()[0].Name, Arguments: map[string]any{}}}, "resp_2", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = broker.FindByAnyCallIDs([]string{firstCalls[0].CallID, secondCalls[0].CallID})
	if err == nil || !strings.Contains(err.Error(), "different pending batches") {
		t.Fatalf("error = %v, want different pending batches", err)
	}
}

func TestCompletionAfterDeadlineRunsAllExpiryCleanup(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	call := &Call{OpenAIID: "call_1", outCh: make(chan string, 1), errCh: make(chan error, 1)}
	aborted := make(chan struct{}, 1)
	hooked := make(chan struct{}, 1)
	batch := &Batch{
		ExpiresAt: time.Now().Add(-time.Second),
		calls:     map[string]*Call{"call_1": call},
		ctx:       ctx,
		cancel:    cancel,
		abort:     func() { aborted <- struct{}{} },
		expireHooks: []func(*Batch){func(*Batch) {
			hooked <- struct{}{}
		}},
	}
	if err := batch.Complete(map[string]string{"call_1": "late"}); !errors.Is(err, ErrExpired) {
		t.Fatalf("Complete error = %v, want ErrExpired", err)
	}
	select {
	case err := <-call.errCh:
		if !errors.Is(err, ErrExpired) {
			t.Fatalf("call error = %v", err)
		}
	default:
		t.Fatal("pending call was not failed")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("batch context was not canceled")
	}
	select {
	case <-aborted:
	default:
		t.Fatal("abort callback was not invoked")
	}
	select {
	case <-hooked:
	default:
		t.Fatal("expiry hook was not invoked")
	}
}

func TestExpiredBatchIsRemovedFromBroker(t *testing.T) {
	t.Parallel()
	broker := NewBroker(10 * time.Millisecond)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	batch, calls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "", "chat", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if batch.Model != "gpt-test" {
		t.Fatalf("batch model = %q", batch.Model)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if _, err := broker.FindByCallIDs([]string{calls[0].CallID}); err == ErrNotFound {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expired batch remained registered")
}

func TestBatchContextCancellationUnblocksHandler(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, openai.ToolScope{})
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
	waitForSDKCall(t, broker, "call_cancel")
	cancel()
	select {
	case got := <-done:
		if !strings.Contains(got, "canceled") {
			t.Fatalf("handler error = %q, want cancellation", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not unblock after context cancellation")
	}
}

func TestBatchCompleteUnblocksHandler(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	params := json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup", Parameters: params}}}, openai.ToolScope{})
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
	_, callID := waitForSDKCall(t, broker, "call_1")
	batch, err := broker.FindByCallIDs([]string{callID})
	if err != nil {
		t.Fatal(err)
	}
	if err := batch.Complete(map[string]string{callID: "ok"}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got != "ok" {
			t.Fatalf("got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not unblock")
	}
}

// TestParallelToolCallsRoundTripThroughBatch proves that two concurrent tool
// invocations are captured in a single batch and that one Complete call unblocks
// both handlers with their respective outputs — the core of parallel tool-call
// support.
func TestParallelToolCallsRoundTripThroughBatch(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, openai.ToolScope{})
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
	proxyIDs := make(map[string]string, 2)
	for _, sdkID := range []string{"call_1", "call_2"} {
		found, proxyID := waitForSDKCall(t, broker, sdkID)
		proxyIDs[sdkID] = proxyID
		if batch != nil && found.ID != batch.ID {
			t.Fatalf("both concurrent calls were not grouped into one batch: %q vs %q", batch.ID, found.ID)
		}
		batch = found
	}
	if _, err := broker.FindByCallIDs([]string{proxyIDs["call_1"], proxyIDs["call_2"]}); err != nil {
		t.Fatalf("both concurrent calls were not grouped into one batch: %v", err)
	}

	if err := batch.Complete(map[string]string{proxyIDs["call_1"]: "out-1", proxyIDs["call_2"]: "out-2"}); err != nil {
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
