package httpapi

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

// failingResponseStreamGateway commits a stream and then fails it, which is
// what an upstream 502 looks like once SSE headers are on the wire.
type failingResponseStreamGateway struct {
	unimplementedGateway
}

func (g *failingResponseStreamGateway) StreamResponse(_ context.Context, _ copilotgw.ResponseRequest) (<-chan copilotgw.ResponseStreamEvent, error) {
	ch := make(chan copilotgw.ResponseStreamEvent, 2)
	go func() {
		defer close(ch)
		ch <- copilotgw.ResponseStreamEvent{Kind: "delta", ItemID: "msg_final", Delta: "partial"}
		ch <- copilotgw.ResponseStreamEvent{Kind: "error", Error: apierr.Upstream("copilot upstream exploded")}
	}()
	return ch, nil
}

type failingChatStreamGateway struct {
	unimplementedGateway
}

func (g *failingChatStreamGateway) StreamChat(_ context.Context, _ copilotgw.ChatRequest) (<-chan copilotgw.StreamEvent, error) {
	ch := make(chan copilotgw.StreamEvent, 2)
	go func() {
		defer close(ch)
		ch <- copilotgw.StreamEvent{Kind: "delta", Delta: "partial"}
		ch <- copilotgw.StreamEvent{Kind: "error", Error: apierr.Upstream("copilot upstream exploded")}
	}()
	return ch, nil
}

type completingChatStreamGateway struct {
	unimplementedGateway
}

func (g *completingChatStreamGateway) StreamChat(_ context.Context, req copilotgw.ChatRequest) (<-chan copilotgw.StreamEvent, error) {
	ch := make(chan copilotgw.StreamEvent, 1)
	go func() {
		defer close(ch)
		ch <- copilotgw.StreamEvent{Kind: "result", Result: &copilotgw.TurnResult{ID: req.OpenAIID, Created: openai.UnixNow(), Model: req.Model, FinishReason: "stop", Text: ""}}
	}()
	return ch, nil
}

// A stream that fails after its headers are committed is legitimately HTTP 200,
// so status alone makes an upstream failure storm indistinguishable from
// healthy traffic. The access line has to carry the terminal outcome, and the
// record has to be severe enough to find.
func TestFailedResponsesStreamIsRecordedInTheAccessLog(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	s := New(config.Config{}, &failingResponseStreamGateway{}, slog.New(slog.NewJSONHandler(&buf, nil)))
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi","stream":true}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the stream was already committed): %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "event: response.failed") {
		t.Fatalf("stream did not fail; body:\n%s", rec.Body.String())
	}
	entry := findLogEntry(t, buf.String(), "request completed")
	if entry["stream_outcome"] != "failed" {
		t.Fatalf("access log stream_outcome = %#v, want \"failed\" (logs:\n%s)", entry["stream_outcome"], buf.String())
	}
	if entry["level"] != "ERROR" {
		t.Fatalf("access log level = %#v, want ERROR for a failed stream", entry["level"])
	}
	if msg, _ := entry["stream_error"].(string); !strings.Contains(msg, "copilot upstream exploded") {
		t.Fatalf("access log stream_error = %#v, want the upstream message", entry["stream_error"])
	}
}

func TestFailedChatStreamIsRecordedInTheAccessLog(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	s := New(config.Config{}, &failingChatStreamGateway{}, slog.New(slog.NewJSONHandler(&buf, nil)))
	rec := httptest.NewRecorder()

	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the stream was already committed): %s", rec.Code, rec.Body.String())
	}
	entry := findLogEntry(t, buf.String(), "request completed")
	if entry["stream_outcome"] != "failed" {
		t.Fatalf("access log stream_outcome = %#v, want \"failed\" (logs:\n%s)", entry["stream_outcome"], buf.String())
	}
	if entry["level"] != "ERROR" {
		t.Fatalf("access log level = %#v, want ERROR for a failed stream", entry["level"])
	}
}

// The healthy case must stay quiet and stay INFO, or the failed case is not
// distinguishable after all.
func TestCompletedStreamsAreRecordedAsCompleted(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name    string
		gateway copilotgw.HTTPGateway
		path    string
		body    string
	}{
		{name: "responses", gateway: &codexStreamGateway{}, path: "/v1/responses", body: `{"model":"gpt-5","input":"hi","stream":true}`},
		{name: "chat", gateway: &completingChatStreamGateway{}, path: "/v1/chat/completions", body: `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			s := New(config.Config{}, tt.gateway, slog.New(slog.NewJSONHandler(&buf, nil)))
			rec := httptest.NewRecorder()

			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body)))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			entry := findLogEntry(t, buf.String(), "request completed")
			if entry["stream_outcome"] != "completed" {
				t.Fatalf("access log stream_outcome = %#v, want \"completed\" (logs:\n%s)", entry["stream_outcome"], buf.String())
			}
			if entry["level"] != "INFO" {
				t.Fatalf("access log level = %#v, want INFO for a healthy stream", entry["level"])
			}
		})
	}
}
