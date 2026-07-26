package copilotgw

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	copilot "github.com/github/copilot-sdk/go"
)

// The acceptance criterion for streamed tool-call arguments is that they
// reconcile: whatever the client accumulated from the fragments has to be
// exactly the arguments of the call it is finally handed. This drives a turn
// that plans one ordinary tool call and one strict one, and asserts both that
// the ordinary call's fragments add up and that the strict call produced none -
// strict arguments are validated before the client sees them, so a fragment of
// one is a promise this proxy may still refuse to keep.
func TestToolCallDeltasStreamNonStrictCallsAndBufferStrictOnes(t *testing.T) {
	t.Parallel()
	broker := toolproxy.NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	strict := true
	rt, err := toolproxy.NewRequestTools(broker, []openai.Tool{
		{Type: "function", Function: openai.FunctionTool{Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`)}},
		{Type: "function", Function: openai.FunctionTool{Name: "verify", Strict: &strict, Parameters: json.RawMessage(`{"type":"object","properties":{"code":{"type":"string"}}}`)}},
	}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan copilot.SessionEvent, 8)
	runner, unpinned := newLoopTestRunner(events, time.Minute)
	runner.kind = "chat"
	runner.rt = rt
	chat := make(chan StreamEvent, 32)
	runner.enableChatStream(chat, nil)

	go runner.loop(&RealGateway{})
	events <- copilot.SessionEvent{Data: &copilot.AssistantTurnStartData{}}
	for _, fragment := range []string{`{"q":`, `"al`, `pha"}`} {
		events <- copilot.SessionEvent{Data: toolCallDelta("sdk_1", "lookup", fragment)}
	}
	events <- copilot.SessionEvent{Data: toolCallDelta("sdk_2", "verify", `{"code":"123"}`)}
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageData{ToolRequests: []copilot.AssistantMessageToolRequest{
		{ToolCallID: "sdk_1", Name: "lookup", Arguments: map[string]any{"q": "alpha"}},
		{ToolCallID: "sdk_2", Name: "verify", Arguments: map[string]any{"code": "123"}},
	}}}

	fragments := map[string]string{}
	names := map[string]string{}
	var result *TurnResult
	for ev := range chat {
		switch ev.Kind {
		case "tool_call_delta":
			fragments[ev.ToolCallID] += ev.Delta
			names[ev.ToolCallID] = ev.ToolName
		case "result":
			result = ev.Result
		case "error":
			t.Fatalf("stream failed: %v", ev.Error)
		}
	}
	if result == nil || len(result.ToolCalls) != 2 {
		t.Fatalf("terminal result = %#v, want two tool calls", result)
	}
	lookup, verify := result.ToolCalls[0], result.ToolCalls[1]
	if got := fragments[lookup.ID]; got != lookup.Function.Arguments {
		t.Fatalf("accumulated fragments for %s = %q, want the final arguments %q", lookup.ID, got, lookup.Function.Arguments)
	}
	if names[lookup.ID] != "lookup" {
		t.Fatalf("streamed tool name = %q, want lookup", names[lookup.ID])
	}
	if _, streamed := fragments[verify.ID]; streamed {
		t.Fatalf("strict tool %s streamed arguments before they were validated", verify.ID)
	}
	if len(fragments) != 1 {
		t.Fatalf("streamed fragments for %d calls, want only the non-strict one: %#v", len(fragments), fragments)
	}

	runner.abort()
	awaitLoopExit(t, runner, unpinned)
}

// A fragment that cannot be resolved back to a declared tool cannot be checked
// for strictness either, so it must not be forwarded on a guess.
func TestToolCallDeltasWithoutAToolNameAreNotForwarded(t *testing.T) {
	t.Parallel()
	broker := toolproxy.NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	rt, err := toolproxy.NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan copilot.SessionEvent, 4)
	runner, unpinned := newLoopTestRunner(events, time.Minute)
	runner.kind = "chat"
	runner.rt = rt
	chat := make(chan StreamEvent, 8)
	runner.enableChatStream(chat, nil)

	go runner.loop(&RealGateway{})
	events <- copilot.SessionEvent{Data: &copilot.AssistantToolCallDeltaData{ToolCallID: "sdk_1", InputDelta: `{"q":"alpha"}`}}
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageData{ToolRequests: []copilot.AssistantMessageToolRequest{
		{ToolCallID: "sdk_1", Name: "lookup", Arguments: map[string]any{"q": "alpha"}},
	}}}

	for ev := range chat {
		if ev.Kind == "tool_call_delta" {
			t.Fatalf("forwarded a fragment for an unidentifiable tool call: %#v", ev)
		}
	}

	runner.abort()
	awaitLoopExit(t, runner, unpinned)
}

// The Responses surface needs the item a fragment extends, and that item has to
// be the same one the terminal response carries - otherwise the client is left
// holding an item that never completes.
func TestToolCallDeltasAnnounceTheItemTheTerminalResponseCarries(t *testing.T) {
	t.Parallel()
	broker := toolproxy.NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	rt, err := toolproxy.NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan copilot.SessionEvent, 4)
	runner, unpinned := newLoopTestRunner(events, time.Minute)
	runner.rt = rt
	runner.setResponseParams(responseParams{id: "resp_1", model: "gpt-test"})
	stream := make(chan ResponseStreamEvent, 16)
	runner.enableResponseStream(stream, nil)

	go runner.loop(&RealGateway{})
	events <- copilot.SessionEvent{Data: toolCallDelta("sdk_1", "lookup", `{"q":"alpha"}`)}
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageData{ToolRequests: []copilot.AssistantMessageToolRequest{
		{ToolCallID: "sdk_1", Name: "lookup", Arguments: map[string]any{"q": "alpha"}},
	}}}

	var itemID, fragments string
	var resp *openai.Response
	for ev := range stream {
		switch ev.Kind {
		case "tool_call_delta":
			if ev.Item == nil || ev.Item.ID != ev.ItemID || ev.Item.Status != "in_progress" {
				t.Fatalf("tool call delta item = %#v, want the in-progress item named by item_id", ev.Item)
			}
			itemID = ev.ItemID
			fragments += ev.Delta
		case "response":
			resp = ev.Response
		case "error":
			t.Fatalf("stream failed: %v", ev.Error)
		}
	}
	if resp == nil {
		t.Fatal("stream produced no terminal response")
	}
	var found *openai.ResponseOutputItem
	for i := range resp.Output {
		if resp.Output[i].ID == itemID {
			found = &resp.Output[i]
		}
	}
	if found == nil {
		t.Fatalf("terminal output %#v does not contain the streamed item %q", resp.Output, itemID)
	}
	if !strings.HasPrefix(found.Type, "function_call") {
		t.Fatalf("streamed item type = %q, want function_call", found.Type)
	}
	if fragments != found.Arguments {
		t.Fatalf("accumulated fragments = %q, want the item's arguments %q", fragments, found.Arguments)
	}

	runner.abort()
	awaitLoopExit(t, runner, unpinned)
}

func toolCallDelta(sdkID, toolName, fragment string) *copilot.AssistantToolCallDeltaData {
	toolType := copilot.AssistantMessageToolRequestTypeFunction
	return &copilot.AssistantToolCallDeltaData{ToolCallID: sdkID, ToolName: &toolName, ToolType: &toolType, InputDelta: fragment}
}
