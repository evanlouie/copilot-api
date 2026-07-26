package toolproxy

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
	copilot "github.com/github/copilot-sdk/go"
)

// A streamed fragment names its call before the call exists, so the id
// reserved for the fragments has to be the id the finished call is published
// under. Two ids would leave the client holding argument fragments for a call
// it never receives.
func TestReservedStreamingCallIDIsTheIDTheCallIsPublishedUnder(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	reserved, ok := rt.ReserveStreamingCall("sdk_1", "lookup", false)
	if !ok {
		t.Fatal("ReserveStreamingCall declined an ordinary function tool")
	}
	if reserved.Kind != toolcatalog.ToolKindFunction || reserved.Name != "lookup" {
		t.Fatalf("reserved = %#v, want a function call named lookup", reserved)
	}
	// A repeat fragment for the same call must not mint a second identity.
	if again, _ := rt.ReserveStreamingCall("sdk_1", "lookup", false); again.CallID != reserved.CallID {
		t.Fatalf("second reservation = %q, want the first one %q", again.CallID, reserved.CallID)
	}

	_, calls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "sdk_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"alpha"}`)}}, "resp_1", "response", "gpt-test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].CallID != reserved.CallID {
		t.Fatalf("captured %#v, want the reserved call id %q", calls, reserved.CallID)
	}
}

// Strict is exactly where the validity contract lives: validateStrictArguments
// refuses a call whose arguments do not satisfy the declared schema, and it can
// only decide that once they are complete. Reserving a strict call for
// streaming would publish arguments this proxy may still refuse.
func TestReserveStreamingCallDeclinesStrictAndUnknownTools(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	strict := true
	rt, err := NewRequestTools(broker, []openai.Tool{
		{Type: "function", Function: openai.FunctionTool{Name: "verify", Strict: &strict, Parameters: json.RawMessage(`{"type":"object","properties":{"code":{"type":"string"}}}`)}},
	}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.ReserveStreamingCall("sdk_1", "verify", false); ok {
		t.Fatal("ReserveStreamingCall accepted a strict tool")
	}
	if _, ok := rt.ReserveStreamingCall("sdk_2", "unconfigured", false); ok {
		t.Fatal("ReserveStreamingCall accepted a tool this request never declared")
	}
	if _, ok := rt.ReserveStreamingCall("", "verify", false); ok {
		t.Fatal("ReserveStreamingCall accepted a call it could never correlate")
	}
}

// A freeform custom tool is declared to the SDK behind customToolSchema's
// synthetic {"input": ...} wrapper, so the model emits that envelope with the
// input JSON-escaped inside it while the custom_tool_call item carries the raw
// input customInput unwraps. Envelope fragments can never add up to the item,
// so the call is not reservable at all - and neither is a tool-search call,
// which has no incremental event to carry.
func TestReserveStreamingCallDeclinesEveryKindButFunction(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	rt, err := NewResponseRequestTools(broker, []toolcatalog.NormalizedTool{
		{Kind: toolcatalog.ToolKindCustom, Name: "apply_patch"},
		{Kind: toolcatalog.ToolKindFunction, Name: "lookup"},
	}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.ReserveStreamingCall("sdk_1", "apply_patch", false); ok {
		t.Fatal("ReserveStreamingCall accepted a freeform custom tool")
	}
	// A function-declared tool the model invokes as custom takes the same route:
	// the SDK's type discriminator is the one CaptureRequests applies too.
	if _, ok := rt.ReserveStreamingCall("sdk_2", "lookup", true); ok {
		t.Fatal("ReserveStreamingCall accepted a call the model typed as custom")
	}
	if _, ok := rt.ReserveStreamingCall("sdk_3", "lookup", false); !ok {
		t.Fatal("ReserveStreamingCall declined an ordinary function call")
	}
}

// Copilot backends reuse low-entropy tool-call ids such as "call_1". When one
// comes round inside a still-open batch, ensureCall answers with the call it
// already minted, so reserving a second identity would leave the client
// accumulating fragments under an id that is never published.
func TestReserveStreamingCallDeclinesAnIDTheBatchAlreadyPublished(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"alpha"}`)}}, "resp_1", "response", "gpt-test", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := rt.ReserveStreamingCall("call_1", "lookup", false); ok {
		t.Fatal("ReserveStreamingCall minted a second identity for an id the batch already owns")
	}
}

// The claim a batch has on a tool-call id has to end when the batch does.
//
// rt.batch is never cleared at a turn boundary, only replaced by the next
// CaptureRequests, so a guard that ignored the batch's state would still see
// turn one's completed batch holding "call_1" on turn two and decline. On a
// backend that restarts its counter every turn - the very behaviour the guard
// exists for - that silently switches streaming off for the whole rest of the
// tool loop.
func TestStreamingResumesOnTheNextTurnWhenTheBackendRecyclesCallIDs(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}

	// Turn one: the model streams fragments for "call_1", then requests it.
	firstReserved, ok := rt.ReserveStreamingCall("call_1", "lookup", false)
	if !ok {
		t.Fatal("ReserveStreamingCall declined the first turn's call")
	}
	firstBatch, firstCalls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"alpha"}`)}}, "resp_1", "response", "gpt-test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstCalls) != 1 || firstCalls[0].CallID != firstReserved.CallID {
		t.Fatalf("turn one captured %#v, want the reserved id %q", firstCalls, firstReserved.CallID)
	}

	// The client answers, which closes the batch and ends the turn.
	if err := firstBatch.Complete(map[string]string{firstCalls[0].CallID: "done"}); err != nil {
		t.Fatal(err)
	}
	if firstBatch.isOpen() {
		t.Fatal("batch is still open after Complete; the rest of this test proves nothing")
	}

	// Turn two: the backend restarts its counter and reuses "call_1".
	secondReserved, ok := rt.ReserveStreamingCall("call_1", "lookup", false)
	if !ok {
		t.Fatal("ReserveStreamingCall declined the second turn; a closed batch still claimed the recycled id")
	}
	if secondReserved.CallID == firstReserved.CallID {
		t.Fatalf("second turn reused the first turn's client-visible id %q", firstReserved.CallID)
	}
	_, secondCalls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: "lookup", Arguments: json.RawMessage(`{"q":"beta"}`)}}, "resp_1", "response", "gpt-test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondCalls) != 1 || secondCalls[0].CallID != secondReserved.CallID {
		t.Fatalf("turn two captured %#v, want the second reservation %q", secondCalls, secondReserved.CallID)
	}
}

// A namespaced Responses function tool has to be announced under its namespace,
// because the terminal item carries one and the two describe the same item.
func TestReserveStreamingCallCarriesTheToolNamespace(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	rt, err := NewResponseRequestTools(broker, []toolcatalog.NormalizedTool{
		{Kind: toolcatalog.ToolKindNamespace, Name: "repo", Children: []toolcatalog.NormalizedTool{{Kind: toolcatalog.ToolKindFunction, Name: "lookup"}}},
	}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	// AvailableTools spells names for the SDK's toolset with a kind prefix; the
	// tool requests the model emits use the plain SDK name.
	sdkName := rt.Tools()[0].Name
	reserved, ok := rt.ReserveStreamingCall("sdk_1", sdkName, false)
	if !ok {
		t.Fatalf("ReserveStreamingCall declined namespaced tool %q", sdkName)
	}
	if reserved.Namespace != "repo" {
		t.Fatalf("reserved = %#v, want namespace repo", reserved)
	}
	_, calls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "sdk_1", Name: sdkName, Arguments: json.RawMessage(`{}`)}}, "resp_1", "response", "gpt-test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Namespace != reserved.Namespace {
		t.Fatalf("captured %#v, want the namespace the reservation announced (%q)", calls, reserved.Namespace)
	}
}

// A repeat request for a call the batch already holds must not swallow the
// reservation: the SDK announces a tool request and separately invokes the
// handler for the same call, and only the first of those mints.
func TestEnsureCallConsumesAReservationOnlyWhenItMints(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	batch := newBatch(time.Minute, "resp_1", "response", "gpt-test", nil, nil, context.Background())
	rt.reserved = map[string]StreamingCall{"sdk_1": {CallID: "call_reserved", Kind: toolcatalog.ToolKindFunction, Name: "lookup"}}
	reserve := rt.reservationFor("sdk_1")
	first := batch.ensureCall("sdk_1", "lookup", ClientTool{}, json.RawMessage(`{}`), "", reserve)
	if first.OpenAIID != "call_reserved" {
		t.Fatalf("minted call id = %q, want the reserved %q", first.OpenAIID, "call_reserved")
	}
	if again := batch.ensureCall("sdk_1", "lookup", ClientTool{}, json.RawMessage(`{}`), "", reserve); again != first {
		t.Fatalf("repeat ensureCall returned a different call %#v", again)
	}
}

// A turn that streams fragments and then aborts never reaches ensureCall, so
// the tool-call boundary and cancellation are the only things that can bound
// the reservation map.
func TestStreamingReservationsAreReleasedAtEveryTurnBoundary(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	// A call the model planned and then never requested.
	if _, ok := rt.ReserveStreamingCall("sdk_abandoned", "lookup", false); !ok {
		t.Fatal("ReserveStreamingCall declined an ordinary function call")
	}
	if _, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "sdk_1", Name: "lookup", Arguments: json.RawMessage(`{}`)}}, "resp_1", "response", "gpt-test", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(rt.reserved) != 0 {
		t.Fatalf("reserved = %#v, want the tool-call boundary to have cleared it", rt.reserved)
	}

	if _, ok := rt.ReserveStreamingCall("sdk_2", "lookup", false); !ok {
		t.Fatal("ReserveStreamingCall declined an ordinary function call")
	}
	rt.CancelCurrent(context.Canceled)
	if len(rt.reserved) != 0 {
		t.Fatalf("reserved = %#v, want cancellation to have cleared it", rt.reserved)
	}
}
