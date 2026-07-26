package copilotgw

import (
	"testing"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
)

func newResponseIdentityRunner(stream chan ResponseStreamEvent, params responseParams) *turnRunner {
	r := &turnRunner{
		model:   params.model,
		kind:    "response",
		created: 1_700_000_000,
		updates: make(chan toolproxy.TurnFinalResult, 4),
	}
	r.setResponseParams(params)
	r.enableResponseStream(stream, nil)
	return r
}

// TestRunnerBuildsOneResponsePerTurn covers invariant 5: a turn's response is
// constructed exactly once, and persistence and the streamed terminal event
// receive that same object. A second construction anywhere - the old shape,
// where the persistence callback and the stream each called responseFromTurn -
// fails this test.
func TestRunnerBuildsOneResponsePerTurn(t *testing.T) {
	t.Parallel()
	stream := make(chan ResponseStreamEvent, 8)
	runner := newResponseIdentityRunner(stream, responseParams{id: "resp_identity", model: "gpt-test", store: true})
	var persisted *openai.Response
	runner.setOnResult(func(turn *TurnResult) error {
		persisted = turn.Response
		return nil
	})

	runner.emitReasoningDelta("think", "rid-1")
	runner.emitDelta("ans")
	res := &TurnResult{ID: "resp_identity", Created: runner.created, Model: "gpt-test", Text: "answer", Reasoning: "thinking", ReasoningID: "rid-1", FinishReason: "stop"}
	runner.emitResult(res)

	if got := res.ResponseBuilds(); got != 1 {
		t.Fatalf("responses built for one turn = %d, want exactly 1", got)
	}
	if persisted == nil {
		t.Fatal("persistence never saw a response")
	}
	streamed := drainForTerminalResponse(t, stream)
	if streamed != persisted || streamed != res.Response {
		t.Fatalf("streamed (%p), persisted (%p) and turn (%p) responses are not the same object", streamed, persisted, res.Response)
	}
}

// TestRunnerAssignsOutputItemIDsBeforeStreamingThem covers invariant 1 at the
// source: the IDs the client sees on deltas are the IDs the built (and
// therefore persisted) response carries.
func TestRunnerAssignsOutputItemIDsBeforeStreamingThem(t *testing.T) {
	t.Parallel()
	stream := make(chan ResponseStreamEvent, 8)
	runner := newResponseIdentityRunner(stream, responseParams{id: "resp_ids", model: "gpt-test", store: true})
	runner.emitReasoningDelta("think", "rid-1")
	runner.emitDelta("ans")
	res := &TurnResult{ID: "resp_ids", Created: runner.created, Model: "gpt-test", Text: "answer", Reasoning: "thinking", ReasoningID: "rid-1", FinishReason: "stop"}
	runner.emitResult(res)

	streamedIDs := map[string]string{}
	terminal := drainForTerminalResponse(t, stream, func(ev ResponseStreamEvent) {
		if ev.Kind == "delta" || ev.Kind == "reasoning_delta" {
			streamedIDs[ev.Kind] = ev.ItemID
		}
	})
	if streamedIDs["reasoning_delta"] != "rs_rid-1" {
		t.Fatalf("streamed reasoning item id = %q, want rs_rid-1", streamedIDs["reasoning_delta"])
	}
	if streamedIDs["delta"] == "" {
		t.Fatal("streamed content delta carried no output item id")
	}
	want := []string{streamedIDs["reasoning_delta"], streamedIDs["delta"]}
	got := make([]string, 0, len(terminal.Output))
	for _, item := range terminal.Output {
		got = append(got, item.ID)
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("terminal output ids = %v, want the streamed ids %v", got, want)
	}
}

// TestResponseCreatedAtComesFromTheTurn pins the timestamp fix: the runner
// stamps the turn's creation time and the terminal response carries it, instead
// of re-reading the clock at emission time and disagreeing with the
// response.created frame.
func TestResponseCreatedAtComesFromTheTurn(t *testing.T) {
	t.Parallel()
	turn := &TurnResult{ID: "resp_created", Created: 1_700_000_042, Text: "answer", FinishReason: "stop"}
	resp := responseFromTurn(responseParams{id: turn.ID, model: "gpt-test", store: true}, turn)
	if resp.CreatedAt != turn.Created {
		t.Fatalf("created_at = %d, want the turn's %d", resp.CreatedAt, turn.Created)
	}
}

// TestTerminalOutputFollowsStreamedItemOrder pins the single source of truth
// for output_index: the terminal output is ordered the way the stream announced
// the items, so a reasoning item that only materialised after content follows
// the message instead of silently taking index 0.
func TestTerminalOutputFollowsStreamedItemOrder(t *testing.T) {
	t.Parallel()
	stream := make(chan ResponseStreamEvent, 8)
	runner := newResponseIdentityRunner(stream, responseParams{id: "resp_order", model: "gpt-test", store: true})
	runner.emitDelta("ans")
	res := &TurnResult{ID: "resp_order", Created: runner.created, Text: "answer", ReasoningEncrypted: "enc", ReasoningID: "rid-late", FinishReason: "stop"}
	runner.emitResult(res)

	terminal := drainForTerminalResponse(t, stream)
	if len(terminal.Output) != 2 || terminal.Output[0].Type != "message" || terminal.Output[1].Type != "reasoning" {
		t.Fatalf("terminal output order = %#v, want the streamed [message, reasoning]", terminal.Output)
	}
}

// TestToolCallTurnResetsOutputItemIdentity covers the runner reuse a
// client-owned tool-call continuation depends on: the continuation turn must
// get its own output items rather than reusing the emitted turn's IDs.
func TestToolCallTurnResetsOutputItemIdentity(t *testing.T) {
	t.Parallel()
	stream := make(chan ResponseStreamEvent, 8)
	runner := newResponseIdentityRunner(stream, responseParams{id: "resp_tools", model: "gpt-test", store: true})
	runner.emitDelta("first")
	first := &TurnResult{ID: "resp_tools", Created: runner.created, Text: "first", FinishReason: "tool_calls", ResponseToolCalls: []toolproxy.CapturedCall{{Kind: toolcatalog.ToolKindFunction, CallID: "call_1", ResponseName: "lookup"}}}
	runner.emitResult(first)

	runner.resetOutputItems()
	// A tool-call result closes the stream it was published on; the continuation
	// request attaches a fresh one.
	continuation := make(chan ResponseStreamEvent, 8)
	runner.enableResponseStream(continuation, nil)
	runner.emitDelta("second")
	second := &TurnResult{ID: "resp_tools", Created: runner.created, Text: "second", FinishReason: "stop"}
	runner.emitResult(second)

	if first.MessageItemID == "" || first.MessageItemID == second.MessageItemID {
		t.Fatalf("continuation turn reused the tool-call turn's message item id %q", first.MessageItemID)
	}
}

func drainForTerminalResponse(t *testing.T, stream <-chan ResponseStreamEvent, observe ...func(ResponseStreamEvent)) *openai.Response {
	t.Helper()
	for {
		select {
		case ev := <-stream:
			for _, fn := range observe {
				fn(ev)
			}
			if ev.Kind == "response" {
				return ev.Response
			}
			if ev.Kind == "error" {
				t.Fatalf("stream failed: %v", ev.Error)
			}
		default:
			t.Fatal("stream ended without a terminal response")
		}
	}
}
