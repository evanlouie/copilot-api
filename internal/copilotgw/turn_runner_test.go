package copilotgw

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	copilot "github.com/github/copilot-sdk/go"
)

// TestFailSendDeliversSessionError verifies the async-Send failure path routes
// the error through the runner loop as a synthetic SessionError event, instead
// of emitting from the Send goroutine. This is what keeps emitError loop-owned
// and free of the send-on-closed race.
func TestFailSendDeliversSessionError(t *testing.T) {
	events := newSessionEventSink(nil)
	r := &turnRunner{closed: make(chan struct{})}

	r.failSend(events, errors.New("boom"))

	select {
	case ev := <-events.events():
		d, ok := ev.Data.(*copilot.SessionErrorData)
		if !ok {
			t.Fatalf("expected *copilot.SessionErrorData, got %T", ev.Data)
		}
		if d.Message != "boom" {
			t.Fatalf("message = %q, want %q", d.Message, "boom")
		}
	default:
		t.Fatal("failSend did not deliver a SessionError event")
	}
}

// TestFailSendUnblocksWhenRunnerClosed ensures a late Send failure cannot block
// its goroutine forever when the loop has already exited (nothing drains the
// event channel and it is already full). The sink must absorb the event and
// return.
func TestFailSendUnblocksWhenRunnerClosed(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	events := newSessionEventSink(nil)
	events.attach(closed)
	fillSessionEventSink(events)
	r := &turnRunner{closed: closed}

	done := make(chan struct{})
	go func() {
		r.failSend(events, errors.New("late"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("failSend blocked even though the runner is closed")
	}
}

func TestStaleRequestGenerationCannotAbortReattachedRunner(t *testing.T) {
	r := &turnRunner{}
	oldGeneration := r.requestGeneration
	r.detachFromRequestContext()
	r.attachToRequestContext()
	if r.shouldAbortForRequestGeneration(oldGeneration) {
		t.Fatal("stale request generation could abort a newer attachment")
	}
	if !r.shouldAbortForRequestGeneration(r.requestGeneration) {
		t.Fatal("current attached generation should abort on cancellation")
	}
}

func TestOnResultCallbackIsConsumedByOneTurn(t *testing.T) {
	r := &turnRunner{updates: make(chan toolproxy.TurnFinalResult, 2)}
	calls := 0
	r.setOnResult(func(*TurnResult) error {
		calls++
		return nil
	})
	r.emitResult(&TurnResult{ID: "resp_first", FinishReason: "stop"})
	r.emitResult(&TurnResult{ID: "resp_second", FinishReason: "stop"})
	if calls != 1 {
		t.Fatalf("onResult called %d times, want exactly once", calls)
	}
}

func TestTurnRunnerRejectsOversizedToolRequestPayload(t *testing.T) {
	events := make(chan copilot.SessionEvent, 1)
	runner := &turnRunner{
		maxOutputBytes: 128,
		events:         events,
		updates:        make(chan toolproxy.TurnFinalResult, 1),
		closed:         make(chan struct{}),
		session:        &copilot.Session{SessionID: "sdk"},
	}
	runner.abortOnce.Do(func() {})
	go runner.loop(&RealGateway{})
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageData{ToolRequests: []copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: "lookup", Arguments: map[string]any{"payload": strings.Repeat("x", 1024)}}}}}
	select {
	case update := <-runner.updates:
		if update.Err == nil || !strings.Contains(update.Err.Error(), "size limit") {
			t.Fatalf("update = %#v", update)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not reject oversized tool payload")
	}
}

// newLoopTestRunner builds a runner whose loop can run without a live copilot
// client: abortOnce is pre-consumed so abort skips the session RPCs, while
// signalAbort still releases the loop. The returned channel is closed when the
// loop releases the runner's sessionstore retention pins.
func newLoopTestRunner(events <-chan copilot.SessionEvent, idleTimeout time.Duration) (*turnRunner, <-chan struct{}) {
	r := &turnRunner{
		ctx:            context.Background(),
		model:          "gpt-test",
		kind:           "response",
		session:        &copilot.Session{SessionID: "sdk_test"},
		events:         events,
		maxOutputBytes: config.DefaultMaxTurnOutputBytes,
		idleTimeout:    idleTimeout,
		updates:        make(chan toolproxy.TurnFinalResult, 4),
		closed:         make(chan struct{}),
		aborted:        make(chan struct{}),
	}
	r.abortOnce.Do(func() {})
	unpinned := make(chan struct{})
	r.addPin(func() { close(unpinned) })
	return r, unpinned
}

func awaitLoopExit(t *testing.T, r *turnRunner, unpinned <-chan struct{}) {
	t.Helper()
	select {
	case <-r.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("runner loop did not terminate")
	}
	select {
	case <-unpinned:
	case <-time.After(2 * time.Second):
		t.Fatal("runner loop exited without releasing its retention pins")
	}
}

func awaitTurnError(t *testing.T, r *turnRunner, want string) {
	t.Helper()
	select {
	case update := <-r.updates:
		if update.Err == nil || !strings.Contains(update.Err.Error(), want) {
			t.Fatalf("update = %#v, want error containing %q", update, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("runner did not emit an error containing %q", want)
	}
}

func awaitTurnResult(t *testing.T, r *turnRunner) *TurnResult {
	t.Helper()
	select {
	case update := <-r.updates:
		if update.Err != nil {
			t.Fatalf("update = %#v, want a turn result", update)
		}
		turn, ok := update.Value.(*TurnResult)
		if !ok {
			t.Fatalf("update value = %T, want *TurnResult", update.Value)
		}
		return turn
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not emit a turn result")
	}
	return nil
}

// awaitStreamedText collects the text the client actually saw, up to the
// terminal event of the response stream.
func awaitStreamedText(t *testing.T, stream <-chan ResponseStreamEvent) string {
	t.Helper()
	var streamed strings.Builder
	for {
		select {
		case ev, ok := <-stream:
			if !ok {
				return streamed.String()
			}
			switch ev.Kind {
			case "delta":
				streamed.WriteString(ev.Delta)
			case "response", "error":
				return streamed.String()
			}
		case <-time.After(2 * time.Second):
			t.Fatal("response stream did not deliver a terminal event")
		}
	}
}

// TestOnResultErrorTerminatesLoop covers the persistence-failure path: a
// SaveResponse error aborts the SDK session, so no further events can arrive and
// the loop must exit instead of parking on its event channel forever. Parking
// would strand the goroutine, the active-registry entry, the client's stream and
// the sessionstore pins that block retention.
func TestOnResultErrorTerminatesLoop(t *testing.T) {
	broker := toolproxy.NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	rt, err := toolproxy.NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan copilot.SessionEvent, 1)
	runner, unpinned := newLoopTestRunner(events, time.Minute)
	runner.rt = rt
	stream := make(chan ResponseStreamEvent, 4)
	runner.enableResponseStream(stream, nil)
	runner.setOnResult(func(*TurnResult) error { return apierr.Internal("failed to persist response") })

	go runner.loop(&RealGateway{})
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageData{ToolRequests: []copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: "lookup", Arguments: map[string]any{"q": "alpha"}}}}}

	awaitTurnError(t, runner, "failed to persist response")
	awaitLoopExit(t, runner, unpinned)
}

// TestAbortTerminatesLoop pins the self-inflicted abort contract: session.Abort
// plus Disconnect stops event delivery, so abort has to be an exit gate for the
// loop. RealGateway.Stop relies on this to drain runners inside its deadline.
func TestAbortTerminatesLoop(t *testing.T) {
	runner, unpinned := newLoopTestRunner(make(chan copilot.SessionEvent), time.Minute)

	go runner.loop(&RealGateway{})
	runner.abort()

	awaitTurnError(t, runner, "aborted")
	awaitLoopExit(t, runner, unpinned)
}

// TestTurnRunnerIdleTimeoutFailsTurn covers a wedged or silently dead SDK
// session: nothing else bounds the wait, so the idle ceiling must fail the turn
// with a real error rather than let the client hang.
func TestTurnRunnerIdleTimeoutFailsTurn(t *testing.T) {
	runner, unpinned := newLoopTestRunner(make(chan copilot.SessionEvent), 20*time.Millisecond)

	go runner.loop(&RealGateway{})

	awaitTurnError(t, runner, "delivered no events")
	awaitLoopExit(t, runner, unpinned)
}

func TestTurnRunnerIdleTimeoutStaysAboveToolCallTTL(t *testing.T) {
	if got := (&RealGateway{}).idleTimeoutForTurns(); got != turnRunnerIdleTimeout {
		t.Fatalf("default idle timeout = %s, want %s", got, turnRunnerIdleTimeout)
	}
	g := &RealGateway{cfg: config.Config{ToolCallTTL: turnRunnerIdleTimeout + time.Hour}}
	if got := g.idleTimeoutForTurns(); got <= g.cfg.ToolCallTTL {
		t.Fatalf("idle timeout = %s, want more than the tool-call TTL %s", got, g.cfg.ToolCallTTL)
	}
}

// TestRequestCancelTerminatesAttachedLoop covers the originating request going
// away while the runner still belongs to it.
func TestRequestCancelTerminatesAttachedLoop(t *testing.T) {
	runner, unpinned := newLoopTestRunner(make(chan copilot.SessionEvent), time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	runner.ctx = ctx

	go runner.loop(&RealGateway{})
	cancel()

	awaitTurnError(t, runner, context.Canceled.Error())
	awaitLoopExit(t, runner, unpinned)
}

// TestRequestCancelDoesNotTerminateDetachedLoop protects the deliberate
// behavior asserted by TestTurnRunnerDetachPreventsRequestCancelAbort: a turn
// parked on client-owned tool calls detaches from its originating request, and
// the follow-up request re-attaches under a new generation. The cancellation of
// the long-gone first request must not end the runner. Closing the event stream
// is then the only remaining exit gate, which also covers the "ended before
// idle" branch.
func TestRequestCancelDoesNotTerminateDetachedLoop(t *testing.T) {
	events := make(chan copilot.SessionEvent)
	runner, unpinned := newLoopTestRunner(events, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	runner.ctx = ctx
	runner.detachFromRequestContext()

	go runner.loop(&RealGateway{})
	cancel()
	// A later request adopts the runner; its own watchContext owns cancellation.
	runner.attachToRequestContext()

	select {
	case <-runner.closed:
		t.Fatal("cancelling the originating request ended a re-attached runner")
	case <-time.After(100 * time.Millisecond):
	}

	close(events)
	awaitTurnError(t, runner, "event stream ended before idle")
	awaitLoopExit(t, runner, unpinned)
}

func TestToolRequestPayloadSizeIncludesArguments(t *testing.T) {
	requests := []copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: "lookup", Arguments: map[string]any{"payload": strings.Repeat("x", 1024)}}}
	size, err := toolRequestPayloadSize(requests)
	if err != nil {
		t.Fatal(err)
	}
	if size <= 1024 {
		t.Fatalf("tool payload size = %d, want serialized metadata plus arguments", size)
	}
}

func TestReasoningAccumulatorReplacesDeltaBufferWithConsolidatedText(t *testing.T) {
	var accumulator reasoningAccumulator
	accumulator.addDelta(strings.Repeat("a", 64), "reasoning")
	accumulator.addConsolidated("final", "reasoning")
	if accumulator.deltas.Len() != 0 || accumulator.resolve() != "final" {
		t.Fatalf("accumulator retained duplicate reasoning: %#v", accumulator)
	}
	if got := accumulator.retainedSizeAfterConsolidated(strings.Repeat("b", 10)); got != 10 {
		t.Fatalf("retained size = %d, want 10", got)
	}
}

func TestCurrentResponseIDUsesContinuationMetadata(t *testing.T) {
	r := &turnRunner{responseID: "resp_initial"}
	if got := r.currentResponseID(); got != "resp_initial" {
		t.Fatalf("currentResponseID without meta = %q, want resp_initial", got)
	}
	r.setCurrentResponseID("resp_nonstream_continuation")
	if got := r.currentResponseID(); got != "resp_nonstream_continuation" {
		t.Fatalf("currentResponseID with non-stream continuation = %q, want resp_nonstream_continuation", got)
	}
	r.setResponseParams(responseParams{id: "resp_stream_continuation", model: "gpt-test", store: true})
	r.enableResponseStream(make(chan ResponseStreamEvent, 1), nil)
	if got := r.currentResponseID(); got != "resp_stream_continuation" {
		t.Fatalf("currentResponseID with stream params = %q, want resp_stream_continuation", got)
	}
}

func TestRunnerCapturesResponseToolCallsWithCurrentResponseID(t *testing.T) {
	broker := toolproxy.NewBroker(time.Minute)
	rt, err := toolproxy.NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	events := newSessionEventSink(nil)
	session := &copilot.Session{SessionID: "sdk_test"}
	runner := (&RealGateway{}).newTurnRunner(context.Background(), "resp_initial", "gpt-test", session, rt, events, t.TempDir(), "response", "resp_initial")
	runner.setCurrentResponseID("resp_continuation")
	events.send(copilot.SessionEvent{Data: &copilot.AssistantMessageData{ToolRequests: []copilot.AssistantMessageToolRequest{{ToolCallID: "call_next", Name: "lookup", Arguments: map[string]any{"q": "alpha"}}}}})

	select {
	case update := <-runner.updates:
		if update.Err != nil {
			t.Fatal(update.Err)
		}
		turn, ok := update.Value.(*TurnResult)
		if !ok || turn.FinishReason != "tool_calls" {
			t.Fatalf("update = %#v, want tool_calls TurnResult", update.Value)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not emit tool-call result")
	}
	batch, err := broker.FindByCallIDs([]string{"call_next"})
	if err != nil {
		t.Fatal(err)
	}
	if batch.ResponseID != "resp_continuation" {
		t.Fatalf("batch.ResponseID = %q, want resp_continuation", batch.ResponseID)
	}
	batch.Cancel(context.Canceled)
	close(events.ch)
}

// TestTurnAccumulatesTextAcrossAssistantMessages covers a turn that produces
// more than one assistant message (the SDK gives each its own MessageID). Every
// message's deltas are forwarded to the client, so the turn result has to carry
// all of them: a result holding only the last message contradicts the stream the
// client already received, and the Responses encoder rejects a terminal text
// that does not extend the streamed content.
//
// The turn is closed with a tool-call message carrying no content because that
// is the only terminal event a loop test can drive: the session-idle path
// disconnects the SDK session, and a runner built for tests has no live
// JSON-RPC client behind its session handle.
func TestTurnAccumulatesTextAcrossAssistantMessages(t *testing.T) {
	broker := toolproxy.NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	rt, err := toolproxy.NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan copilot.SessionEvent, 8)
	runner, unpinned := newLoopTestRunner(events, time.Minute)
	runner.rt = rt
	stream := make(chan ResponseStreamEvent, 8)
	runner.enableResponseStream(stream, nil)

	go runner.loop(&RealGateway{})
	events <- copilot.SessionEvent{Data: &copilot.AssistantTurnStartData{TurnID: "1"}}
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageStartData{MessageID: "msg_a"}}
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageDeltaData{MessageID: "msg_a", DeltaContent: "Alpha "}}
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageData{MessageID: "msg_a", Content: "Alpha "}}
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageStartData{MessageID: "msg_b"}}
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageDeltaData{MessageID: "msg_b", DeltaContent: "Beta"}}
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageData{MessageID: "msg_b", Content: "Beta"}}
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageData{MessageID: "msg_c", ToolRequests: []copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: "lookup", Arguments: map[string]any{"q": "alpha"}}}}}

	turn := awaitTurnResult(t, runner)
	if turn.Text != "Alpha Beta" {
		t.Fatalf("TurnResult.Text = %q, want %q", turn.Text, "Alpha Beta")
	}
	if streamed := awaitStreamedText(t, stream); streamed != turn.Text {
		t.Fatalf("streamed text = %q, terminal text = %q; the terminal text must cover everything the client was shown", streamed, turn.Text)
	}
	runner.abort()
	awaitLoopExit(t, runner, unpinned)
}

// TestToolCallBoundaryResetsAccumulatedText pins the other end of the
// accumulator's lifetime. The loop is reused across a client-owned tool-call
// continuation, so the text emitted with a tool-call result must not leak into
// the next turn. The continuation here arrives without an AssistantTurnStart
// event, which makes the tool boundary the only reset keeping the turns apart.
func TestToolCallBoundaryResetsAccumulatedText(t *testing.T) {
	broker := toolproxy.NewBroker(time.Minute)
	defer broker.CancelAll(context.Canceled)
	rt, err := toolproxy.NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan copilot.SessionEvent, 2)
	runner, unpinned := newLoopTestRunner(events, time.Minute)
	runner.rt = rt

	go runner.loop(&RealGateway{})
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageData{MessageID: "msg_a", Content: "First", ToolRequests: []copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: "lookup", Arguments: map[string]any{"q": "alpha"}}}}}
	if first := awaitTurnResult(t, runner); first.Text != "First" {
		t.Fatalf("tool-call turn text = %q, want %q", first.Text, "First")
	}
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageData{MessageID: "msg_b", Content: "Second", ToolRequests: []copilot.AssistantMessageToolRequest{{ToolCallID: "call_2", Name: "lookup", Arguments: map[string]any{"q": "beta"}}}}}
	if second := awaitTurnResult(t, runner); second.Text != "Second" {
		t.Fatalf("continuation turn text = %q, want %q: the previous turn's text leaked across the tool boundary", second.Text, "Second")
	}
	runner.abort()
	awaitLoopExit(t, runner, unpinned)
}

func TestTurnDebugStatsObserve(t *testing.T) {
	s := newTurnDebugStats()
	s.observeContentDelta("abc")
	d := s.observeContentDelta("de")
	if s.contentDeltaCount != 2 {
		t.Errorf("contentDeltaCount = %d, want 2", s.contentDeltaCount)
	}
	if s.contentDeltaBytes != 5 {
		t.Errorf("contentDeltaBytes = %d, want 5", s.contentDeltaBytes)
	}
	if s.maxContentDeltaBytes != 3 {
		t.Errorf("maxContentDeltaBytes = %d, want 3", s.maxContentDeltaBytes)
	}
	if d.index != 2 || d.cumulativeBytes != 5 || d.maxBytes != 3 {
		t.Errorf("delta stats = %+v, want index=2 cumulative=5 max=3", d)
	}

	r := s.observeReasoningDelta("wxyz")
	if s.reasoningDeltaCount != 1 || s.reasoningDeltaBytes != 4 || s.maxReasoningDeltaBytes != 4 {
		t.Errorf("reasoning stats = count %d bytes %d max %d, want 1/4/4", s.reasoningDeltaCount, s.reasoningDeltaBytes, s.maxReasoningDeltaBytes)
	}
	if r.index != 1 {
		t.Errorf("reasoning delta index = %d, want 1", r.index)
	}

	if attrs := s.summaryAttrs(); len(attrs)%2 != 0 {
		t.Errorf("summaryAttrs returned odd-length attr list: %d", len(attrs))
	}
}

func TestReasoningAccumulatorPrefersConsolidated(t *testing.T) {
	var a reasoningAccumulator
	a.addDelta("think ", "r1")
	a.addDelta("more", "r1")
	if got := a.resolve(); got != "think more" {
		t.Fatalf("delta fallback = %q, want %q", got, "think more")
	}
	a.addConsolidated("final", "r1")
	if got := a.resolve(); got != "final" {
		t.Fatalf("consolidated = %q, want %q", got, "final")
	}
}

// TestReasoningAccumulatorMarkToolBoundaryDropsLateFinal protects the tool-turn
// reasoning reset: after a tool boundary, the just-emitted turn's late final
// reasoning block must not seed the next turn, but a genuinely new turn must.
func TestReasoningAccumulatorMarkToolBoundaryDropsLateFinal(t *testing.T) {
	var a reasoningAccumulator
	a.addDelta("turn1", "r1")
	a.markToolBoundary()
	a.addConsolidated("late final for r1", "r1") // belongs to the prior turn; must be ignored
	if got := a.resolve(); got != "" {
		t.Fatalf("late final leaked into next turn: %q", got)
	}
	a.addDelta("turn2", "r2")
	if got := a.resolve(); got != "turn2" {
		t.Fatalf("next turn reasoning = %q, want %q", got, "turn2")
	}
}

// TestDebugDeltaContentGating asserts per-delta debug logs include sizes but only
// include the raw delta text when COPILOT_LOG_CONTENT (cfg.LogContent) is set.
func TestDebugDeltaContentGating(t *testing.T) {
	const secret = "SECRET_DELTA_CONTENT"
	cases := []struct {
		name        string
		logContent  bool
		wantPreview bool
	}{
		{"redacted", false, false},
		{"preview", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			g := &RealGateway{cfg: config.Config{LogContent: tc.logContent}, log: logger}
			r := &turnRunner{ctx: context.Background(), id: "chatcmpl_x", kind: "chat", model: "m"}
			stats := newTurnDebugStats()
			ds := stats.observeContentDelta(secret)

			r.debugDelta(g, "copilot content delta", secret, ds, "message_id", "m1")

			logs := buf.String()
			if !strings.Contains(logs, "delta_bytes") {
				t.Errorf("expected delta_bytes in log: %s", logs)
			}
			if tc.wantPreview {
				if !strings.Contains(logs, "delta_preview") || !strings.Contains(logs, secret) {
					t.Errorf("expected delta_preview with content when LogContent=true: %s", logs)
				}
			} else {
				if strings.Contains(logs, "delta_preview") {
					t.Errorf("unexpected delta_preview when LogContent=false: %s", logs)
				}
				if strings.Contains(logs, secret) {
					t.Errorf("delta content leaked into logs when LogContent=false: %s", logs)
				}
			}
		})
	}
}
