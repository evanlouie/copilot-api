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

// The SDK's tool-call type is the same discriminator CaptureRequests applies,
// so the kind a fragment is routed under has to match the kind the finished
// item is built as.
func TestReserveStreamingCallMatchesTheCapturedKind(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	rt, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "apply_patch"}}}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	reserved, ok := rt.ReserveStreamingCall("sdk_1", "apply_patch", true)
	if !ok || reserved.Kind != toolcatalog.ToolKindCustom {
		t.Fatalf("reserved = %#v/%t, want a custom call", reserved, ok)
	}
	custom := copilot.AssistantMessageToolRequestTypeCustom
	_, calls, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "sdk_1", Name: "apply_patch", Type: &custom, Arguments: "*** Begin Patch"}}, "resp_1", "response", "gpt-test", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Kind != toolcatalog.ToolKindCustom || calls[0].CallID != reserved.CallID {
		t.Fatalf("captured %#v, want a custom call under %q", calls, reserved.CallID)
	}
	if calls[0].Input != "*** Begin Patch" {
		t.Fatalf("captured input = %q, want the raw custom input the fragments carry", calls[0].Input)
	}
}
