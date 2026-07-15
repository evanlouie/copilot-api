package copilotgw

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

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
	events := make(chan copilot.SessionEvent, 1)
	r := &turnRunner{closed: make(chan struct{})}

	r.failSend(events, errors.New("boom"))

	select {
	case ev := <-events:
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
// its goroutine forever when the loop has already exited (the events channel has
// no reader). The select on r.closed must release it.
func TestFailSendUnblocksWhenRunnerClosed(t *testing.T) {
	events := make(chan copilot.SessionEvent) // unbuffered, no reader
	closed := make(chan struct{})
	close(closed)
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
	r.enableResponseStream(make(chan ResponseStreamEvent, 1), "resp_stream_continuation", "gpt-test", "", nil, true, false, nil)
	if got := r.currentResponseID(); got != "resp_stream_continuation" {
		t.Fatalf("currentResponseID with stream meta = %q, want resp_stream_continuation", got)
	}
}

func TestRunnerCapturesResponseToolCallsWithCurrentResponseID(t *testing.T) {
	broker := toolproxy.NewBroker(time.Minute)
	rt, err := toolproxy.NewRequestTools(broker, []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan copilot.SessionEvent, 4)
	session := &copilot.Session{SessionID: "sdk_test"}
	runner := (&RealGateway{}).newTurnRunner(context.Background(), "resp_initial", "gpt-test", session, rt, events, t.TempDir(), "response", "resp_initial")
	runner.setCurrentResponseID("resp_continuation")
	events <- copilot.SessionEvent{Data: &copilot.AssistantMessageData{ToolRequests: []copilot.AssistantMessageToolRequest{{ToolCallID: "call_next", Name: "lookup", Arguments: map[string]any{"q": "alpha"}}}}}

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
	close(events)
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
