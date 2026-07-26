package httpapi

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// The WebSocket idle watchdog is the one piece of the Responses socket whose
// behaviour is defined purely in terms of elapsed time, which is exactly what an
// end-to-end socket test cannot pin without a wall-clock budget. These tests run
// it inside a synctest bubble instead: the clock is fake and only advances once
// every goroutine is durably blocked, so the assertions below are about the
// watchdog's rules rather than about how promptly a loaded runner scheduled it.

// An in-flight response is not idle, no matter how long the client stays quiet.
// This is the property TestResponsesWebSocketKeepsLongResponseAliveWhileGenerating
// asserts over a real socket.
func TestWebSocketIdleWatchdogNeverFiresWhileAResponseIsGenerating(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const idle = time.Second
		state := &responsesWebSocketState{lastSeen: time.Now()}
		if !state.tryStart() {
			t.Fatal("tryStart refused to start the first response")
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fired := make(chan struct{})
		go watchResponsesWebSocketIdle(ctx, state, idle, func() { close(fired) })

		// Thousands of watchdog ticks pass with the client silent throughout.
		time.Sleep(time.Hour)
		synctest.Wait()
		select {
		case <-fired:
			t.Fatal("idle watchdog aborted a response that was still generating")
		default:
		}

		// Once generation finishes the idle clock restarts from that moment, so
		// the hour of silence above must not count towards it.
		state.finish()
		time.Sleep(idle - time.Nanosecond)
		synctest.Wait()
		select {
		case <-fired:
			t.Fatal("idle watchdog fired before the idle timeout elapsed after generation finished")
		default:
		}

		start := time.Now()
		select {
		case <-fired:
		case <-time.After(idle):
			t.Fatal("idle watchdog never fired after the response finished and the client stayed quiet")
		}
		if waited := time.Since(start); waited > time.Nanosecond {
			t.Fatalf("watchdog fired %v after the idle timeout elapsed, want immediately on the first tick past it", waited)
		}
	})
}

// Client activity restarts the idle clock; only silence should end a connection.
func TestWebSocketIdleWatchdogRestartsOnClientActivity(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const idle = time.Second
		state := &responsesWebSocketState{lastSeen: time.Now()}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fired := make(chan struct{})
		go watchResponsesWebSocketIdle(ctx, state, idle, func() { close(fired) })

		// Keep speaking just inside the timeout for far longer than the timeout.
		for range 20 {
			time.Sleep(idle - idle/4)
			state.markActivity()
		}
		synctest.Wait()
		select {
		case <-fired:
			t.Fatal("idle watchdog fired on a connection the client kept using")
		default:
		}

		select {
		case <-fired:
		case <-time.After(2 * idle):
			t.Fatal("idle watchdog never fired once the client fell silent")
		}
	})
}

// A cancelled connection context must stop the watchdog, or every closed socket
// would leave a ticker goroutine behind.
func TestWebSocketIdleWatchdogStopsWithTheConnection(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		state := &responsesWebSocketState{lastSeen: time.Now()}
		ctx, cancel := context.WithCancel(context.Background())
		fired := make(chan struct{})
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			watchResponsesWebSocketIdle(ctx, state, time.Second, func() { close(fired) })
		}()
		cancel()
		<-stopped
		select {
		case <-fired:
			t.Fatal("idle watchdog reported an idle timeout on a cancelled connection")
		default:
		}
	})
}
