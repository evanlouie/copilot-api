package copilotgw

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	copilot "github.com/github/copilot-sdk/go"
)

// syncBuffer collects log output that can be written from the sink's flusher
// goroutine while the test goroutine reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fillSessionEventSink saturates the sink's channel so the next send has to
// take the spill path.
func fillSessionEventSink(s *sessionEventSink) {
	for i := 0; i < sessionEventBuffer; i++ {
		s.send(copilot.SessionEvent{Data: &copilot.AssistantMessageDeltaData{DeltaContent: "x"}})
	}
}

// TestSessionEventCallbackNeverBlocksStalledConsumer is the regression test for
// the read-loop stall. The SDK dispatches session.event notifications
// synchronously on the single JSON-RPC read-loop goroutine of the process-wide
// client, so a callback that waits on the turn runner takes down every other
// session's events, tool calls and RPC replies. The runner here never drains -
// the state a loop is in while it waits inside session.Disconnect, whose reply
// has to travel back over that very read loop - and the callback must still
// return.
func TestSessionEventCallbackNeverBlocksStalledConsumer(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	sink := newSessionEventSink(slog.New(slog.NewJSONHandler(logs, nil)))
	done := make(chan struct{})
	defer close(done)
	sink.attach(done)

	onEvent := (&RealGateway{}).sessionRuntimeDefaults(true, sink).onEvent

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		for i := 0; i < sessionEventBuffer+sessionEventOverflow+16; i++ {
			onEvent(copilot.SessionEvent{Data: &copilot.AssistantMessageDeltaData{DeltaContent: fmt.Sprintf("delta-%d", i)}})
		}
	}()

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("copilot session event callback blocked on a consumer that stopped draining")
	}
	// Everything up to the channel plus the spill buffer is accepted (the
	// flusher may also hold one in flight); the rest is dropped rather than
	// parked on the read loop.
	dropped := sink.droppedEvents()
	if dropped == 0 || dropped > 16 {
		t.Fatalf("droppedEvents = %d, want between 1 and 16", dropped)
	}
	if !strings.Contains(logs.String(), "copilot session event buffer overflowed") {
		t.Fatalf("overflow was not reported: %s", logs.String())
	}
}

// TestSessionEventSinkFailsTurnAfterOverflow asserts dropped events are never
// silent: the consumer is handed a terminal session error so the runner fails
// the turn instead of waiting for events it will never see.
func TestSessionEventSinkFailsTurnAfterOverflow(t *testing.T) {
	t.Parallel()
	sink := newSessionEventSink(nil)
	done := make(chan struct{})
	defer close(done)
	sink.attach(done)

	for i := 0; i < sessionEventBuffer+sessionEventOverflow+64 && sink.droppedEvents() == 0; i++ {
		sink.send(copilot.SessionEvent{Data: &copilot.AssistantMessageDeltaData{DeltaContent: "x"}})
	}
	if sink.droppedEvents() == 0 {
		t.Fatal("sink never overflowed")
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-sink.events():
			if data, ok := event.Data.(*copilot.SessionErrorData); ok {
				if data.Message != sessionEventOverflowMessage {
					t.Fatalf("terminal error = %q, want %q", data.Message, sessionEventOverflowMessage)
				}
				return
			}
		case <-deadline:
			t.Fatal("overflowing sink never delivered a terminal session error")
		}
	}
}

// TestSessionEventSinkBuffersWhileConsumerIsAbsent covers the parked
// WarmResponseSession: its channel has no reader until the follow-up request
// attaches a runner, so events must be buffered rather than dropped.
func TestSessionEventSinkBuffersWhileConsumerIsAbsent(t *testing.T) {
	t.Parallel()
	sink := newSessionEventSink(nil)
	const total = sessionEventBuffer + 64

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		for i := 0; i < total; i++ {
			sink.send(copilot.SessionEvent{Data: &copilot.AssistantMessageDeltaData{DeltaContent: fmt.Sprintf("delta-%d", i)}})
		}
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("copilot session event callback blocked while the warm session was parked")
	}
	if dropped := sink.droppedEvents(); dropped != 0 {
		t.Fatalf("droppedEvents = %d, want 0 while buffering a parked warm session", dropped)
	}

	done := make(chan struct{})
	defer close(done)
	sink.attach(done)
	for i := 0; i < total; i++ {
		select {
		case event := <-sink.events():
			data, ok := event.Data.(*copilot.AssistantMessageDeltaData)
			if !ok {
				t.Fatalf("event %d = %T, want *copilot.AssistantMessageDeltaData", i, event.Data)
			}
			if want := fmt.Sprintf("delta-%d", i); data.DeltaContent != want {
				t.Fatalf("event %d = %q, want %q", i, data.DeltaContent, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("buffered event %d was never delivered", i)
		}
	}
}

// TestSessionEventSinkReportsDropsAfterConsumerFinished checks the other
// terminal state: once the runner loop is gone nothing can consume the spill,
// so it is released and the loss is logged instead of stalling the read loop.
func TestSessionEventSinkReportsDropsAfterConsumerFinished(t *testing.T) {
	t.Parallel()
	logs := &syncBuffer{}
	sink := newSessionEventSink(slog.New(slog.NewJSONHandler(logs, nil)))
	done := make(chan struct{})
	close(done)
	sink.attach(done)
	fillSessionEventSink(sink)

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		sink.send(copilot.SessionEvent{Data: &copilot.SessionIdleData{}})
	}()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("copilot session event callback blocked after the runner finished")
	}

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "dropped buffered copilot session events") {
		if time.Now().After(deadline) {
			t.Fatal("dropped events were never reported")
		}
		time.Sleep(time.Millisecond)
	}
	if dropped := sink.droppedEvents(); dropped == 0 {
		t.Fatal("droppedEvents = 0, want the released spill to be counted")
	}
}
