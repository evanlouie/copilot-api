package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// SSEWriter serializes every frame on one event stream.
//
// The mutex is not decoration: the keep-alive goroutine below writes to the
// same http.ResponseWriter as the handler that owns the stream, and two
// concurrent writers would interleave halves of two frames into one unparseable
// event.
type SSEWriter struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	mu       sync.Mutex
	lastByte time.Time
}

func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	return &SSEWriter{w: w, flusher: flusher, lastByte: time.Now()}, true
}

func (s *SSEWriter) Data(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.DataJSON(b)
}

func (s *SSEWriter) DataJSON(b []byte) error {
	return s.writeFrame("data: %s\n\n", b)
}

func (s *SSEWriter) Done() error {
	return s.writeFrame("data: [DONE]\n\n")
}

func (s *SSEWriter) Event(event string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.EventJSON(event, b)
}

func (s *SSEWriter) EventJSON(event string, b []byte) error {
	return s.writeFrame("event: %s\ndata: %s\n\n", event, b)
}

// writeFrame is the single point every byte of the stream goes through, so the
// lock and the idle clock cannot be forgotten at a call site.
func (s *SSEWriter) writeFrame(format string, args ...any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.w, format, args...); err != nil {
		return err
	}
	s.flusher.Flush()
	s.lastByte = time.Now()
	return nil
}

// idleFor reports whether nothing has been written for at least d.
func (s *SSEWriter) idleFor(d time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.lastByte) >= d
}

// KeepAlive starts emitting a comment frame whenever the stream has been idle
// for interval, and returns the stop func the caller must defer.
//
// A comment (`: keep-alive`) is the event-stream grammar's own no-op: every
// conforming client discards a line beginning with a colon, so this is
// invisible to the caller while still being a byte on the wire. That matters
// because the connection is usually not end-to-end — AWS ALB defaults to a 60s
// idle timeout and Cloudflare to 100s, and both drop a connection with nothing
// in flight regardless of what this process's own timeouts say — and a
// reasoning-heavy turn can legitimately produce nothing for minutes. It also
// gives dead-peer detection: a write to a client that vanished fails within one
// interval rather than at the end of the turn.
//
// stop waits for the goroutine to finish, so once it returns no frame can
// still be in flight. That is what lets a handler defer it: the request logging
// middleware reads the recorder's byte count the moment the handler returns,
// and a keep-alive landing after that point would race it.
//
// interval <= 0 disables it.
func (s *SSEWriter) KeepAlive(ctx context.Context, interval time.Duration) func() {
	if interval <= 0 {
		return func() {}
	}
	// Tick at half the interval so the longest possible gap between bytes is
	// one and a half intervals rather than two.
	tick := interval / 2
	if tick <= 0 {
		tick = interval
	}
	done := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-ticker.C:
				if !s.idleFor(interval) {
					continue
				}
				if err := s.writeFrame(": keep-alive\n\n"); err != nil {
					// The peer is gone. The handler will discover the same thing
					// on its next frame; there is nothing useful to do here.
					return
				}
			}
		}
	}()
	return func() {
		stopOnce.Do(func() { close(done) })
		wg.Wait()
	}
}
