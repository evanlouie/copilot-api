package copilotgw

import (
	"log/slog"
	"sync"

	copilot "github.com/github/copilot-sdk/go"
)

const (
	// sessionEventBuffer is the capacity of the channel a turnRunner drains.
	sessionEventBuffer = 256
	// sessionEventOverflow bounds the spill buffer that absorbs events while
	// nothing is draining the channel. A parked WarmResponseSession has no
	// reader at all until its follow-up request arrives, so the spill has to be
	// generous enough to buffer instead of drop, yet bounded so a wedged
	// consumer cannot grow it without limit.
	sessionEventOverflow = 1024
	// sessionEventOverflowMessage is delivered to the runner loop as a terminal
	// SessionError once the spill buffer is exhausted, so the turn fails cleanly
	// instead of waiting for events that were dropped.
	sessionEventOverflowMessage = "copilot session event buffer overflowed; turn aborted to keep the copilot connection alive"
)

// sessionEventSink decouples the copilot SDK session event callback from the
// turnRunner that consumes the events.
//
// The SDK dispatches session.event notifications synchronously on the JSON-RPC
// read-loop goroutine, and there is exactly one read loop for the process-wide
// copilot.Client. A blocking send from the callback therefore stalls every
// other session's events, tool invocations and RPC replies - including the
// session.destroy reply that Session.Disconnect waits for, which the runner
// loop calls before it stops draining. send must return promptly in all cases:
// it publishes directly while the consumer keeps up, spills into a bounded
// buffer drained by a flusher goroutine while the consumer is stalled or
// absent, and once that buffer is exhausted it stops accepting events and
// queues a terminal session error so the turn fails rather than hangs.
type sessionEventSink struct {
	ch  chan copilot.SessionEvent
	log *slog.Logger

	mu       sync.Mutex
	done     <-chan struct{}
	pending  []copilot.SessionEvent
	flushing bool
	stopped  bool
	dropped  int
}

func newSessionEventSink(log *slog.Logger) *sessionEventSink {
	return &sessionEventSink{ch: make(chan copilot.SessionEvent, sessionEventBuffer), log: log}
}

// events is the receive side handed to the turnRunner loop.
func (s *sessionEventSink) events() <-chan copilot.SessionEvent {
	if s == nil {
		return nil
	}
	return s.ch
}

// attach records the consumer liveness channel (turnRunner.closed) and releases
// anything buffered while the sink had no reader. Until a consumer attaches the
// sink can only buffer, because nothing would ever unblock a delivery attempt.
func (s *sessionEventSink) attach(done <-chan struct{}) {
	if s == nil || done == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done = done
	s.startFlushLocked()
}

// send is the SDK's onEvent callback. It never blocks.
func (s *sessionEventSink) send(e copilot.SessionEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.stopped {
		s.dropped++
		s.mu.Unlock()
		return
	}
	// Spilled events must stay ahead of new ones, so only publish directly when
	// the flusher owns nothing.
	if !s.flushing && len(s.pending) == 0 {
		select {
		case s.ch <- e:
			s.mu.Unlock()
			return
		default:
		}
	}
	if len(s.pending) < sessionEventOverflow {
		s.pending = append(s.pending, e)
		s.startFlushLocked()
		s.mu.Unlock()
		return
	}
	// The consumer stopped draining for good. Give up on this event and queue a
	// terminal error instead, so the runner fails the turn rather than waiting
	// on events that will never be delivered.
	s.stopped = true
	s.dropped++
	s.pending = append(s.pending, copilot.SessionEvent{Data: &copilot.SessionErrorData{Message: sessionEventOverflowMessage}})
	s.startFlushLocked()
	log := s.log
	s.mu.Unlock()
	if log != nil {
		log.Error("copilot session event buffer overflowed", "buffer", sessionEventBuffer, "overflow", sessionEventOverflow)
	}
}

// startFlushLocked hands the spill buffer to a goroutine so that waiting on the
// consumer never happens on the SDK read loop.
func (s *sessionEventSink) startFlushLocked() {
	if s.flushing || s.done == nil || len(s.pending) == 0 {
		return
	}
	s.flushing = true
	go s.flush()
}

func (s *sessionEventSink) flush() {
	for {
		s.mu.Lock()
		if len(s.pending) == 0 {
			s.flushing = false
			s.mu.Unlock()
			return
		}
		event := s.pending[0]
		s.pending = s.pending[1:]
		done := s.done
		s.mu.Unlock()
		select {
		case s.ch <- event:
		case <-done:
			s.discard(1)
			return
		}
	}
}

// discard abandons the spill buffer once the consumer is finished. inFlight
// counts the event the flusher had already dequeued.
func (s *sessionEventSink) discard(inFlight int) {
	s.mu.Lock()
	lost := inFlight + len(s.pending)
	s.pending = nil
	s.flushing = false
	s.stopped = true
	s.dropped += lost
	log := s.log
	s.mu.Unlock()
	if log != nil && lost > 0 {
		log.Warn("dropped buffered copilot session events after the turn finished", "events", lost)
	}
}

// droppedEvents reports how many events the sink could not deliver.
func (s *sessionEventSink) droppedEvents() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}
