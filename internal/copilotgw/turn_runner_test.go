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
