package copilotgw

import (
	"context"
	"testing"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
)

func TestResolveReasoningPrefersConsolidated(t *testing.T) {
	t.Parallel()
	if got := resolveReasoning("final", "deltas"); got != "final" {
		t.Fatalf("resolveReasoning(final, deltas) = %q, want final", got)
	}
	if got := resolveReasoning("", "deltas"); got != "deltas" {
		t.Fatalf("resolveReasoning(empty, deltas) = %q, want deltas fallback", got)
	}
	if got := resolveReasoning("", ""); got != "" {
		t.Fatalf("resolveReasoning(empty, empty) = %q, want empty", got)
	}
}

func TestTurnRunnerDetachPreventsRequestCancelAbort(t *testing.T) {
	t.Parallel()
	r := &turnRunner{}
	if !r.shouldAbortForRequestContext() {
		t.Fatal("fresh runner should abort on request cancellation")
	}
	r.detachFromRequestContext()
	if r.shouldAbortForRequestContext() {
		t.Fatal("detached runner should not abort on request cancellation")
	}
	r.attachToRequestContext()
	if !r.shouldAbortForRequestContext() {
		t.Fatal("reattached runner should abort on request cancellation")
	}
}

func TestResponseFromTurnPrependsReasoningItem(t *testing.T) {
	t.Parallel()
	turn := &TurnResult{
		Text:               "the answer",
		Reasoning:          "let me think",
		ReasoningEncrypted: "enc-blob",
		ReasoningID:        "rid-7",
		FinishReason:       "stop",
	}
	resp := responseFromTurn(responseParams{id: "resp_1", model: "claude-sonnet-4.6", store: true}, turn)
	if len(resp.Output) != 2 {
		t.Fatalf("output length = %d, want reasoning + message: %#v", len(resp.Output), resp.Output)
	}
	reasoning := resp.Output[0]
	if reasoning.Type != "reasoning" || reasoning.ID != "rs_rid-7" || reasoning.EncryptedContent != "enc-blob" {
		t.Fatalf("reasoning item = %#v", reasoning)
	}
	if len(reasoning.Summary) != 1 || reasoning.Summary[0].Type != "summary_text" || reasoning.Summary[0].Text != "let me think" {
		t.Fatalf("reasoning summary = %#v", reasoning.Summary)
	}
	if resp.Output[1].Type != "message" {
		t.Fatalf("second item = %#v, want message after reasoning", resp.Output[1])
	}
}

func TestResponseFromTurnWithoutReasoningHasNoReasoningItem(t *testing.T) {
	t.Parallel()
	turn := &TurnResult{Text: "hi", FinishReason: "stop"}
	resp := responseFromTurn(responseParams{id: "resp_1", model: "gpt-5", store: true}, turn)
	for _, item := range resp.Output {
		if item.Type == "reasoning" {
			t.Fatalf("unexpected reasoning item: %#v", resp.Output)
		}
	}
}

// TestResponseFromTurnAlwaysIncludesReasoningItem pins the durable half of the
// reasoning-emission split: responseFromTurn builds the object that is both
// returned and persisted, so it always carries the turn's reasoning. Hiding
// reasoning is the edge's job - see the wire-level coverage in
// internal/httpapi (TestResponsesNonStreamingReasoningEmissionOffSuppressesReasoning,
// TestResponsesStreamReasoningEmissionOffSuppressesReasoning and
// TestReasoningEmissionOffFiltersStoredRecordOnRead).
func TestResponseFromTurnAlwaysIncludesReasoningItem(t *testing.T) {
	t.Parallel()
	turn := &TurnResult{Text: "hi", Reasoning: "hidden", ReasoningEncrypted: "enc", FinishReason: "stop"}
	resp := responseFromTurn(responseParams{id: "resp_1", model: "gpt-5", store: true}, turn)
	if len(resp.Output) != 2 || resp.Output[0].Type != "reasoning" || resp.Output[1].Type != "message" {
		t.Fatalf("output = %#v, want reasoning then message", resp.Output)
	}
	if resp.Output[0].EncryptedContent != "enc" {
		t.Fatalf("reasoning item = %#v, want encrypted content preserved for the stored record", resp.Output[0])
	}
}

func TestResponseFromTurnReasoningPrecedesToolCalls(t *testing.T) {
	t.Parallel()
	turn := &TurnResult{
		Reasoning:    "consider the tool",
		ReasoningID:  "rid-9",
		FinishReason: "tool_calls",
		ToolCalls: []openai.ChatToolCall{{
			ID:       "call_1",
			Type:     "function",
			Function: openai.ToolCallFunction{Name: "lookup", Arguments: `{"q":"x"}`},
		}},
	}
	resp := responseFromTurn(responseParams{id: "resp_1", model: "claude-sonnet-4.6", store: true}, turn)
	if len(resp.Output) != 2 || resp.Output[0].Type != "reasoning" || resp.Output[1].Type != "function_call" {
		t.Fatalf("output = %#v, want reasoning then function_call", resp.Output)
	}
}

// TestReasoningAccumulatorResetPreventsTurnLeak guards the interleaved-thinking
// invariant: the runner loop is reused across the client-owned tool-call
// continuation, so reasoning state must not leak (or concatenate) between turns.
func TestReasoningAccumulatorResetPreventsTurnLeak(t *testing.T) {
	t.Parallel()
	var acc reasoningAccumulator

	// Turn 1: consolidated reasoning "A" plus streaming deltas, then a tool call.
	acc.deltas.WriteString("A-delta")
	acc.consolidated = "A"
	acc.opaque = "opaque-A"
	acc.encrypted = "enc-A"
	acc.id = "rid-A"
	res1 := &TurnResult{}
	if got := acc.resolve(); got != "A" {
		t.Fatalf("turn 1 resolve = %q, want A", got)
	}
	acc.applyTo(res1)
	if res1.ReasoningID != "rid-A" || res1.ReasoningEncrypted != "enc-A" {
		t.Fatalf("turn 1 applyTo = %#v", res1)
	}

	// Tool-call boundary.
	acc.markToolBoundary()
	if acc.resolve() != "" || acc.id != "" || acc.opaque != "" || acc.encrypted != "" {
		t.Fatalf("tool boundary did not clear state: %#v", acc)
	}

	// The SDK can send the final reasoning block for turn 1 after the tool-call
	// message. It must be ignored rather than seeding turn 2.
	acc.addConsolidated("A-late-final", "rid-A")
	if got := acc.resolve(); got != "" {
		t.Fatalf("late final reasoning leaked after tool boundary: %q", got)
	}

	// Turn 2: only streaming deltas (no consolidated block, as can happen on a
	// continuation turn). Must resolve to the fresh deltas, never the stale "A"
	// and never the concatenation "A-deltaB-delta".
	acc.addDelta("B-delta", "rid-B")
	res2 := &TurnResult{}
	if got := acc.resolve(); got != "B-delta" {
		t.Fatalf("turn 2 resolve = %q, want B-delta (no leak/concat)", got)
	}
	acc.applyTo(res2)
	if res2.ReasoningID != "rid-B" || res2.ReasoningEncrypted != "" || res2.ReasoningOpaque != "" {
		t.Fatalf("turn 2 applyTo leaked turn 1 state: %#v", res2)
	}
}

// TestResponsesPersistReasoningItemRoundTrip ensures a stored response keeps its
// reasoning item (summary + encrypted content + output order) through
// SaveResponse -> GetResponse.
func TestResponsesPersistReasoningItemRoundTrip(t *testing.T) {
	t.Parallel()
	store := sessionstore.New(t.TempDir(), t.TempDir())
	gw := NewReal(config.Config{}, store, nil)

	turn := &TurnResult{
		Text:               "the answer",
		Reasoning:          "let me think",
		ReasoningEncrypted: "enc-blob",
		ReasoningID:        "rid-7",
		FinishReason:       "stop",
	}
	resp := responseFromTurn(responseParams{id: "resp_persist", model: "claude-sonnet-4.6", store: true}, turn)
	record := recordFromResponse(resp, "sdk-session", "")
	if err := store.SaveResponse(record); err != nil {
		t.Fatal(err)
	}

	got, err := gw.GetResponse(context.Background(), "resp_persist")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Output) != 2 || got.Output[0].Type != "reasoning" || got.Output[1].Type != "message" {
		t.Fatalf("round-tripped output = %#v, want reasoning then message", got.Output)
	}
	reasoning := got.Output[0]
	if reasoning.ID != "rs_rid-7" || reasoning.EncryptedContent != "enc-blob" {
		t.Fatalf("reasoning item lost fields: %#v", reasoning)
	}
	if len(reasoning.Summary) != 1 || reasoning.Summary[0].Text != "let me think" {
		t.Fatalf("reasoning summary lost: %#v", reasoning.Summary)
	}
}
