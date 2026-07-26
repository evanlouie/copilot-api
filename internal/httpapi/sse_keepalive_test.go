package httpapi

import (
	"bufio"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

// silentThenAnsweringGateway models a reasoning-heavy turn: nothing at all for a
// while, then a complete response. Held open until release is closed.
type silentThenAnsweringGateway struct {
	copilotgw.Gateway
	release chan struct{}
}

func (g *silentThenAnsweringGateway) StreamResponse(ctx context.Context, req copilotgw.ResponseRequest) (<-chan copilotgw.ResponseStreamEvent, error) {
	ch := make(chan copilotgw.ResponseStreamEvent, 2)
	go func() {
		defer close(ch)
		select {
		case <-g.release:
		case <-ctx.Done():
			return
		}
		ch <- copilotgw.ResponseStreamEvent{Kind: "delta", ItemID: "msg_final", Delta: "ok"}
		ch <- copilotgw.ResponseStreamEvent{Kind: "response", Response: &openai.Response{ID: req.ResponseID, Object: openai.ObjectResponse, CreatedAt: openai.UnixNow(), Status: "completed", Model: req.Model, OutputText: "ok", Output: []openai.ResponseOutputItem{{ID: "msg_final", Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: "ok"}}}}, ParallelToolCalls: true, Store: req.Store}}
	}()
	return ch, nil
}

// An intermediary drops a connection with no bytes in flight regardless of what
// this process's own timeouts say: AWS ALB defaults to 60s idle, Cloudflare to
// 100s. A turn that thinks in silence therefore has to be kept warm with a
// comment frame, which the event-stream grammar defines as a no-op.
func TestIdleSSEStreamEmitsKeepAliveComments(t *testing.T) {
	gateway := &silentThenAnsweringGateway{release: make(chan struct{})}
	s := New(config.Config{SSEKeepAliveInterval: 20 * time.Millisecond}, gateway, slog.Default())
	hts := httptest.NewServer(s.Handler())
	defer hts.Close()

	req, err := http.NewRequest(http.MethodPost, hts.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	reader := bufio.NewReader(resp.Body)
	lines := make(chan string, 256)
	go func() {
		defer close(lines)
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				lines <- line
			}
			if err != nil {
				return
			}
		}
	}()

	// The gateway is still silent, so any comment frame that arrives can only be
	// a keep-alive.
	deadline := time.After(10 * time.Second)
	sawKeepAlive := false
	for !sawKeepAlive {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("stream ended before any keep-alive")
			}
			if strings.HasPrefix(line, ":") {
				if line != ": keep-alive\n" {
					t.Fatalf("unexpected comment frame %q", line)
				}
				sawKeepAlive = true
			}
		case <-deadline:
			t.Fatal("no keep-alive frame on a stream that produced nothing for 10s")
		}
	}

	close(gateway.release)
	var body strings.Builder
	done := time.After(10 * time.Second)
	for !strings.HasSuffix(body.String(), "data: [DONE]\n\n") {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stream ended before [DONE]:\n%s", body.String())
			}
			body.WriteString(line)
		case <-done:
			t.Fatalf("stream never reached [DONE]:\n%s", body.String())
		}
	}
	if !strings.Contains(body.String(), "event: response.completed") {
		t.Fatalf("stream did not complete after the keep-alives:\n%s", body.String())
	}
}

// A keep-alive must never appear inside another frame, and a stream that is
// busy must not carry any at all.
func TestKeepAliveDoesNotInterleaveWithEventFrames(t *testing.T) {
	rec := httptest.NewRecorder()
	writer, ok := NewSSEWriter(rec)
	if !ok {
		t.Fatal("recorder is not a flusher")
	}
	stop := writer.KeepAlive(context.Background(), time.Millisecond)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 25 {
				if err := writer.Event("response.output_text.delta", map[string]any{"delta": strings.Repeat("x", 64)}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	stop()

	for _, frame := range strings.Split(strings.TrimSuffix(rec.Body.String(), "\n\n"), "\n\n") {
		lines := strings.Split(frame, "\n")
		switch {
		case len(lines) == 1 && lines[0] == ": keep-alive":
		case len(lines) == 2 && strings.HasPrefix(lines[0], "event: ") && strings.HasPrefix(lines[1], "data: "):
		default:
			t.Fatalf("interleaved SSE frame %q", frame)
		}
	}
}

// The ticker must not outlive the stream, on any exit path. stop() waits for
// the goroutine, so once it returns no frame can still land on a writer the
// handler has already finished with.
func TestKeepAliveStopsWithTheStream(t *testing.T) {
	for _, test := range []struct {
		name string
		exit func(cancel context.CancelFunc, stop func())
	}{
		{name: "explicit stop", exit: func(_ context.CancelFunc, stop func()) { stop() }},
		{name: "context cancelled", exit: func(cancel context.CancelFunc, stop func()) { cancel(); stop() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writer, ok := NewSSEWriter(rec)
			if !ok {
				t.Fatal("recorder is not a flusher")
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stop := writer.KeepAlive(ctx, time.Millisecond)
			time.Sleep(10 * time.Millisecond)
			test.exit(cancel, stop)

			before := rec.Body.Len()
			time.Sleep(10 * time.Millisecond)
			if rec.Body.Len() != before {
				t.Fatal("keep-alive kept writing after the stream ended")
			}
			stop() // idempotent: a deferred stop may run after an explicit one
		})
	}
}

// The keep-alive frame must be invisible to a client, which is what the
// comment grammar is for. Both SSE parsers in this package already skip it, so
// this pins that they do.
func TestKeepAliveCommentIsSkippedByTheStreamParsers(t *testing.T) {
	body := ": keep-alive\n\nevent: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0}\n\n: keep-alive\n\ndata: [DONE]\n\n"
	events := parseResponseStreamEvents(t, body)
	if len(events) != 1 || events[0].Type != "response.created" {
		t.Fatalf("parsed events = %#v", events)
	}
	assertParsableSSE(t, body)
}
