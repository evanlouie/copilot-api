package toolproxy

import (
	"encoding/json"
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
	unknown, err := handler(copilot.PermissionRequestCustomTool{ToolName: "unknown_alias"}, copilot.PermissionInvocation{})
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
	batch, _ := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: rt.Tools()[0].Name, Arguments: map[string]any{}}}, "", "chat", make(chan TurnFinalResult, 1), nil)
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
