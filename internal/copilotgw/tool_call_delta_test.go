package copilotgw

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	copilot "github.com/github/copilot-sdk/go"
)

// This is a premise test, not a regression guard for the reconciler. It proves
// the thing toolArgumentsSuffix is built on: that a streamed fragment stream
// and the finished arguments genuinely do diverge byte-wise. toolproxy.rawArgs
// re-encodes the SDK's decoded map[string]any, so encoding/json sorts the keys,
// escapes the angle brackets and reformats the float, while the fragments are
// the model's own bytes that none of that applies to. The payload is chosen to
// trip every one of those normalizations at once.
//
// The reconciler itself is guarded in internal/httpapi, by the
// toolArgumentsSuffix table in stream_reconcile_test.go and by the two
// end-to-end "...MatchOnlyAsJSON" stream tests. This test would still pass if
// the reconciler regressed, because it compares the two sides with its own
// sameJSON helper rather than calling it.
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
// holding an item that never completes, or one whose identity changes under it
// between `added` and `done`. The tool here is namespaced because `namespace`
// is exactly the kind of field that is easy to set on the terminal item and
// forget on the announced one.
func TestToolCallDeltasAnnounceTheItemTheTerminalResponseCarries(t *testing.T) {
	t.Parallel()
	broker := toolproxy.NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	rt, err := toolproxy.NewResponseRequestTools(broker, []toolcatalog.NormalizedTool{
		{Kind: toolcatalog.ToolKindNamespace, Name: "repo", Children: []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindFunction, Name: "lookup"}}},
	}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	sdkName := rt.Tools()[0].Name
	events := make(chan copilot.SessionEvent, 4)
	runner, unpinned := newLoopTestRunner(events, time.Minute)
	runner.rt = rt
	runner.setResponseParams(responseParams{id: "resp_1", model: "gpt-test"})
	stream := make(chan ResponseStreamEvent, 16)
	runner.enableResponseStream(stream, nil)

	go runner.loop(&RealGateway{})
	events <- copilot.SessionEvent{Data: toolCallDelta("sdk_1", sdkName, `{"q":"alpha"}`)}
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageData{ToolRequests: []copilot.AssistantMessageToolRequest{
		{ToolCallID: "sdk_1", Name: sdkName, Arguments: map[string]any{"q": "alpha"}},
	}}}

	var announced *openai.ResponseOutputItem
	var itemID, fragments string
	var resp *openai.Response
	for ev := range stream {
		switch ev.Kind {
		case "tool_call_delta":
			if ev.Item == nil || ev.Item.ID != ev.ItemID || ev.Item.Status != "in_progress" {
				t.Fatalf("tool call delta item = %#v, want the in-progress item named by item_id", ev.Item)
			}
			announced = ev.Item
			itemID = ev.ItemID
			fragments += ev.Delta
		case "response":
			resp = ev.Response
		case "error":
			t.Fatalf("stream failed: %v", ev.Error)
		}
	}
	if resp == nil || announced == nil {
		t.Fatalf("stream produced response=%v announced=%v", resp != nil, announced != nil)
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
	if fragments != found.Arguments {
		t.Fatalf("accumulated fragments = %q, want the item's arguments %q", fragments, found.Arguments)
	}
	// Everything that identifies the item has to survive from `added` to `done`.
	// Only the status and the arguments are allowed to differ, because those are
	// what the fragments fill in.
	settled := *announced
	settled.Status = found.Status
	settled.Arguments = found.Arguments
	if !reflect.DeepEqual(settled, *found) {
		t.Fatalf("announced item %#v does not settle into the terminal item %#v", *announced, *found)
	}
	if found.Namespace != "repo" {
		t.Fatalf("terminal item namespace = %q, want repo; the test tool is no longer namespaced", found.Namespace)
	}

	runner.abort()
	awaitLoopExit(t, runner, unpinned)
}

func toolCallDelta(sdkID, toolName, fragment string) *copilot.AssistantToolCallDeltaData {
	toolType := copilot.AssistantMessageToolRequestTypeFunction
	return &copilot.AssistantToolCallDeltaData{ToolCallID: sdkID, ToolName: &toolName, ToolType: &toolType, InputDelta: fragment}
}

// sameJSON reports whether two JSON texts denote the same value. It duplicates
// what httpapi.sameJSONValue does rather than sharing it, because this package
// is establishing the premise that reconciler rests on, and must not depend on
// the reconciler being correct.
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
