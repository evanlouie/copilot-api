package toolproxy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"

	copilot "github.com/github/copilot-sdk/go"
)

// TestCollidingSDKToolCallIDsStayIsolatedPerRequest pins the tool-call id the
// proxy hands to clients to the proxy itself.
//
// copilot.AssistantMessageToolRequest.ToolCallID is minted by the upstream
// model, not by this proxy, and the backends behind Copilot are not uniform:
// several emit low-entropy sequential ids such as "call_1" or "tool_0". When
// that id is reused verbatim as the key of the process-global Broker.byCall
// map, two concurrent requests that both produce "call_1" collide: the second
// Register overwrites the first, and FindByCallIDs then hands one client's
// continuation the *other* client's pending batch. Same-arity collisions
// deliver client A's tool output into client B's conversation, so this is a
// cross-request confidentiality bug rather than a crash.
//
// The proxy must therefore mint its own id per call. The SDK's id is kept only
// on Call.SDKID, which is what reunites a later SDK tool invocation with the
// call already published on the wire.
func TestCollidingSDKToolCallIDsStayIsolatedPerRequest(t *testing.T) {
	t.Parallel()
	broker := NewBroker(time.Minute)

	rtA, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	rtB, err := NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}

	// Both upstream turns hand back the same low-entropy SDK tool-call id.
	const sdkID = "call_1"

	batchA, callsA, err := rtA.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: sdkID, Name: "lookup", Arguments: map[string]any{"who": "a"}}}, "resp_a", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	batchB, callsB, err := rtB.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: sdkID, Name: "lookup", Arguments: map[string]any{"who": "b"}}}, "resp_b", "response", "gpt-test", make(chan TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(callsA) != 1 || len(callsB) != 1 {
		t.Fatalf("captured %d and %d calls, want one each", len(callsA), len(callsB))
	}

	idA, idB := callsA[0].CallID, callsB[0].CallID
	if idA == idB {
		t.Fatalf("both requests published tool_call_id %q; the model-supplied id leaked onto the wire and into the process-global broker map", idA)
	}
	for _, id := range []string{idA, idB} {
		if !strings.HasPrefix(id, "call_") || len(id) != len("call_")+36 {
			t.Fatalf("published tool_call_id %q is not a proxy-minted call_<uuid>", id)
		}
	}

	// Outbound direction: each published id must resolve to its own batch.
	foundA, err := broker.FindByCallIDs([]string{idA})
	if err != nil {
		t.Fatalf("lookup of request A's tool_call_id failed: %v", err)
	}
	if foundA.ID != batchA.ID {
		t.Fatalf("request A's tool_call_id resolved to batch %q, want %q", foundA.ID, batchA.ID)
	}
	foundB, err := broker.FindByCallIDs([]string{idB})
	if err != nil {
		t.Fatalf("lookup of request B's tool_call_id failed: %v", err)
	}
	if foundB.ID != batchB.ID {
		t.Fatalf("request B's tool_call_id resolved to batch %q, want %q", foundB.ID, batchB.ID)
	}

	// Inbound direction: the proxy id the client echoes back must still carry
	// the SDK's own id, which is how the pending SDK tool invocation is answered.
	callA, okA := batchA.callBySDKID(sdkID)
	callB, okB := batchB.callBySDKID(sdkID)
	if !okA || !okB {
		t.Fatalf("batches lost the reverse SDK-id mapping: A=%t B=%t", okA, okB)
	}
	if callA.OpenAIID != idA || callB.OpenAIID != idB {
		t.Fatalf("reverse mapping resolved to the wrong calls: A=%q (want %q), B=%q (want %q)", callA.OpenAIID, idA, callB.OpenAIID, idB)
	}
	if callA.SDKID != sdkID || callB.SDKID != sdkID {
		t.Fatalf("SDK ids did not round-trip: A=%q B=%q, want %q", callA.SDKID, callB.SDKID, sdkID)
	}

	// Completing one request must not answer the other's pending SDK call.
	if err := batchA.Complete(map[string]string{idA: "output-for-a"}); err != nil {
		t.Fatalf("completing request A failed: %v", err)
	}
	out, err := callA.wait(context.Background())
	if err != nil {
		t.Fatalf("request A's SDK call was not answered: %v", err)
	}
	if out != "output-for-a" {
		t.Fatalf("request A's SDK call got %q, want output-for-a", out)
	}
	if !batchB.isOpen() {
		t.Fatal("completing request A also closed request B's batch")
	}

	if err := batchB.Complete(map[string]string{idB: "output-for-b"}); err != nil {
		t.Fatalf("completing request B failed: %v", err)
	}
	outB, err := callB.wait(context.Background())
	if err != nil {
		t.Fatalf("request B's SDK call was not answered: %v", err)
	}
	if outB != "output-for-b" {
		t.Fatalf("request B's SDK call got %q, want output-for-b", outB)
	}
}
