package copilotgw

// streamSink is one transport's live event channel plus the liveness signal
// that lets the runner stop publishing to a client that has gone away.
//
// Chat Completions and Responses carry different event types but need identical
// plumbing: attach a channel, publish unless the consumer is gone, report
// whether the event landed, and close exactly once. Keeping that in one generic
// type means a fix to the send/close discipline - which is subtle, because the
// runner loop is the sole owner of both operations - cannot reach one surface
// and miss the other.
type streamSink[T any] struct {
	ch   chan<- T
	done <-chan struct{}
}

// attach binds a consumer's channel. A nil channel detaches the sink, which
// also drops the stale liveness signal so a later attach cannot inherit it.
func (s *streamSink[T]) attach(ch chan<- T, done <-chan struct{}) {
	s.ch = ch
	if ch == nil {
		s.done = nil
		return
	}
	s.done = done
}

// take detaches the sink and returns what it held, so a caller holding the
// runner lock can publish or close outside it.
func (s *streamSink[T]) take() streamSink[T] {
	held := *s
	s.ch = nil
	s.done = nil
	return held
}

func (s streamSink[T]) active() bool { return s.ch != nil }

// send publishes an event and reports whether it was delivered. A sink with no
// liveness signal blocks until the consumer reads, which is what a runner built
// directly in a test wants.
func (s streamSink[T]) send(ev T) bool {
	if s.ch == nil {
		return false
	}
	if s.done == nil {
		s.ch <- ev
		return true
	}
	select {
	case s.ch <- ev:
		return true
	case <-s.done:
		return false
	}
}

func (s streamSink[T]) close() {
	if s.ch != nil {
		close(s.ch)
	}
}
