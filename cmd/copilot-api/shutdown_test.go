package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"
)

// stubWebSocketShutdowner stands in for *httpapi.Server. Closing started gives
// tests a happens-after signal that shutdownServer has begun draining, so
// shutdown ordering can be observed without sleeping.
type stubWebSocketShutdowner struct {
	started   chan struct{}
	holdUntil bool
}

func (s *stubWebSocketShutdowner) Shutdown(ctx context.Context) error {
	close(s.started)
	if !s.holdUntil {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newShutdownTestServer starts an http.Server whose request contexts descend
// from requestRoot, exactly as serve wires BaseContext.
func newShutdownTestServer(t *testing.T, requestRoot context.Context, handler http.HandlerFunc) (*http.Server, string, chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return requestRoot },
	}
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv, "http://" + listener.Addr().String(), serveErrCh
}

func TestShutdownServerDrainsInFlightStreamBeforeCancelling(t *testing.T) {
	requestRoot, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	started := make(chan struct{})
	release := make(chan struct{})
	handlerCtxErr := make(chan error, 1)
	srv, baseURL, serveErrCh := newShutdownTestServer(t, requestRoot, func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			handlerCtxErr <- errors.New("response writer is not a flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: chunk\n\n")
		flusher.Flush()
		close(started)
		select {
		case <-r.Context().Done():
			// Shutdown cancelled the request root mid-stream: the client would
			// see a truncated body with no terminal event.
			handlerCtxErr <- fmt.Errorf("cancelled mid-stream: %w", r.Context().Err())
			return
		case <-release:
		}
		handlerCtxErr <- r.Context().Err()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	})

	bodyCh := make(chan string, 1)
	go func() {
		response, err := http.Get(baseURL + "/stream")
		if err != nil {
			bodyCh <- "request failed: " + err.Error()
			return
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			bodyCh <- "read failed: " + err.Error()
			return
		}
		bodyCh <- string(body)
	}()
	<-started

	webSockets := &stubWebSocketShutdowner{started: make(chan struct{})}
	shutdownErrCh := make(chan error, 1)
	go func() {
		shutdownErrCh <- shutdownServer(discardLogger(), srv, webSockets, serveErrCh, make(chan os.Signal), cancelRequests, time.Minute)
	}()
	<-webSockets.started
	if err := requestRoot.Err(); err != nil {
		t.Fatalf("request root cancelled before draining: %v", err)
	}
	close(release)

	if err := <-handlerCtxErr; err != nil {
		t.Fatalf("in-flight request was not drained: %v", err)
	}
	if err := <-shutdownErrCh; err != nil {
		t.Fatalf("shutdownServer error = %v", err)
	}
	if body := <-bodyCh; body != "data: chunk\n\ndata: [DONE]\n\n" {
		t.Fatalf("stream body = %q", body)
	}
}

func TestShutdownServerEscalatesOnSecondSignal(t *testing.T) {
	requestRoot, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	started := make(chan struct{})
	returned := make(chan struct{})
	srv, baseURL, serveErrCh := newShutdownTestServer(t, requestRoot, func(w http.ResponseWriter, r *http.Request) {
		defer close(returned)
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		// Only an escalation can release this handler, so the graceful drain
		// cannot complete on its own.
		<-r.Context().Done()
	})

	go func() {
		response, err := http.Get(baseURL + "/hang")
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()
	<-started

	signals := make(chan os.Signal, 1)
	webSockets := &stubWebSocketShutdowner{started: make(chan struct{})}
	shutdownErrCh := make(chan error, 1)
	go func() {
		// An hour of grace: only the second signal can end this shutdown.
		shutdownErrCh <- shutdownServer(discardLogger(), srv, webSockets, serveErrCh, signals, cancelRequests, time.Hour)
	}()
	<-webSockets.started
	signals <- syscall.SIGINT

	<-returned
	if err := <-shutdownErrCh; err != nil {
		t.Fatalf("shutdownServer error = %v", err)
	}
	if requestRoot.Err() == nil {
		t.Fatal("second signal did not cancel in-flight request contexts")
	}
}

func TestShutdownServerCancelsRequestsWhenGraceExpires(t *testing.T) {
	requestRoot, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	started := make(chan struct{})
	returned := make(chan struct{})
	srv, baseURL, serveErrCh := newShutdownTestServer(t, requestRoot, func(w http.ResponseWriter, r *http.Request) {
		defer close(returned)
		w.WriteHeader(http.StatusOK)
		close(started)
		<-r.Context().Done()
	})

	go func() {
		response, err := http.Get(baseURL + "/hang")
		if err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}()
	<-started

	webSockets := &stubWebSocketShutdowner{started: make(chan struct{}), holdUntil: true}
	err := shutdownServer(discardLogger(), srv, webSockets, serveErrCh, make(chan os.Signal), cancelRequests, 20*time.Millisecond)
	<-returned
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdownServer error = %v", err)
	}
	if requestRoot.Err() == nil {
		t.Fatal("expired grace period did not cancel in-flight request contexts")
	}
}

func TestAwaitCloseBoundsWait(t *testing.T) {
	closed := make(chan struct{})
	close(closed)
	if !awaitClose(closed, time.Minute) {
		t.Fatal("awaitClose reported a timeout for an already closed channel")
	}
	blocked := make(chan struct{})
	defer close(blocked)
	if awaitClose(blocked, time.Millisecond) {
		t.Fatal("awaitClose reported completion for a channel that never closed")
	}
}
