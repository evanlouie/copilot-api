package copilotgw

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	copilot "github.com/github/copilot-sdk/go"
)

// The acceptance criterion for streamed tool-call arguments is that they
// reconcile with the call the client is finally handed. The payload here is
// chosen to break a byte-wise test on purpose: toolproxy.rawArgs re-encodes the
// SDK's decoded map[string]any, so encoding/json sorts the keys, escapes the
// angle brackets and reformats the float. The fragments are the model's own
// bytes and none of those normalizations apply to them, so the two agree as
// JSON values and disagree as bytes - which is exactly the property the HTTP
// layer's toolArgumentsSuffix has to be built on.
//
// The strict call in the same turn asserts the other half: strict arguments are
// validated before the client sees them, so a fragment of one is a promise this
// proxy may still refuse to keep.
func TestToolCallDeltasStreamNonStrictCallsAndBufferStrictOnes(t *testing.T) {
	t.Parallel()
	broker := toolproxy.NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	strict := true
	rt, err := toolproxy.NewRequestTools(broker, []openai.Tool{
		{Type: "function", Function: openai.FunctionTool{Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`)}},
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
	for _, fragment := range []string{`{"temp": 21.0, `, `"location": "Paris", `, `"note": "a <b> c"}`} {
		events <- copilot.SessionEvent{Data: toolCallDelta("sdk_1", "lookup", fragment)}
	}
	events <- copilot.SessionEvent{Data: toolCallDelta("sdk_2", "verify", `{"code":"123"}`)}
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageData{ToolRequests: []copilot.AssistantMessageToolRequest{
		{ToolCallID: "sdk_1", Name: "lookup", Arguments: map[string]any{"temp": 21.0, "location": "Paris", "note": "a <b> c"}},
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
	streamed := fragments[lookup.ID]
	if !sameJSON(t, streamed, lookup.Function.Arguments) {
		t.Fatalf("accumulated fragments %q are not the final arguments %q", streamed, lookup.Function.Arguments)
	}
	// Guards the guard: a payload that happened to round-trip byte for byte would
	// make this test pass without exercising the normalization at all.
	if streamed == lookup.Function.Arguments {
		t.Fatalf("fragments %q round-tripped unchanged; the payload no longer exercises rawArgs normalization", streamed)
	}
	if names[lookup.ID] != "lookup" {
		t.Fatalf("streamed tool name = %q, want lookup", names[lookup.ID])
	}
	if _, streamedStrict := fragments[verify.ID]; streamedStrict {
		t.Fatalf("strict tool %s streamed arguments before they were validated", verify.ID)
	}
	if len(fragments) != 1 {
		t.Fatalf("streamed fragments for %d calls, want only the non-strict one: %#v", len(fragments), fragments)
	}

	runner.abort()
	awaitLoopExit(t, runner, unpinned)
}

// A freeform custom tool is declared to the SDK behind a synthetic
// {"input": ...} wrapper, so the model streams that envelope while the
// custom_tool_call item carries the unwrapped raw input. The fragments could
// not add up to the item, so none are forwarded.
func TestCustomToolCallDeltasAreNotForwarded(t *testing.T) {
	t.Parallel()
	broker := toolproxy.NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	rt, err := toolproxy.NewResponseRequestTools(broker, []toolcatalog.NormalizedTool{
		{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch"},
	}, openai.ToolScope{})
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
	custom := copilot.AssistantMessageToolRequestTypeCustom
	name := "apply_patch"
	events <- copilot.SessionEvent{Data: &copilot.AssistantToolCallDeltaData{ToolCallID: "sdk_1", ToolName: &name, ToolType: &custom, InputDelta: `{"input":"*** Begin Patch\n"}`}}
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageData{ToolRequests: []copilot.AssistantMessageToolRequest{
		{ToolCallID: "sdk_1", Name: "apply_patch", Type: &custom, Arguments: `{"input":"*** Begin Patch\n"}`},
	}}}

	for ev := range stream {
		if ev.Kind == "tool_call_delta" {
			t.Fatalf("forwarded a fragment of a custom tool's synthetic envelope: %#v", ev)
		}
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

// sameJSON reports whether two JSON texts denote the same value, which is the
// equality the HTTP layer reconciles streamed tool arguments on.
func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var left, right any
	if err := json.Unmarshal([]byte(a), &left); err != nil {
		t.Fatalf("accumulated fragments are not valid JSON: %v (%q)", err, a)
	}
	if err := json.Unmarshal([]byte(b), &right); err != nil {
		t.Fatalf("final arguments are not valid JSON: %v (%q)", err, b)
	}
	return reflect.DeepEqual(left, right)
}
