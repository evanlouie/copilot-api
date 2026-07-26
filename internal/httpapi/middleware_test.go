package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
)

// panicOnWriteRecorder records real bytes but panics once on a chosen Write, so
// tests can simulate a handler panic that happens after a stream has already
// been committed to the client.
type panicOnWriteRecorder struct {
	header      http.Header
	body        bytes.Buffer
	status      int
	wroteHeader bool
	writes      int
	panicAt     int
	panicked    bool
}

func newPanicOnWriteRecorder(panicAt int) *panicOnWriteRecorder {
	return &panicOnWriteRecorder{header: http.Header{}, status: http.StatusOK, panicAt: panicAt}
}

func (w *panicOnWriteRecorder) Header() http.Header { return w.header }

func (w *panicOnWriteRecorder) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
}

func (w *panicOnWriteRecorder) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	w.writes++
	if !w.panicked && w.writes == w.panicAt {
		w.panicked = true
		panic("boom after stream commit")
	}
	return w.body.Write(b)
}

func (w *panicOnWriteRecorder) Flush() {}

type panicModelsGateway struct {
	copilotgw.Gateway
}

func (panicModelsGateway) ListModels(context.Context) ([]copilotgw.Model, error) {
	panic("boom in handler")
}

// findLogEntry returns the last JSON log record whose msg equals want.
func findLogEntry(t *testing.T, logs string, want string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if entry["msg"] == want {
			found = entry
		}
	}
	if found == nil {
		t.Fatalf("no %q log record in:\n%s", want, logs)
	}
	return found
}

// assertParsableSSE fails when body contains a line an SSE client would not
// recognise. Per the event-stream grammar a line without a known field name is
// silently discarded, so a raw JSON error object appended to a committed stream
// is invisible to every conforming client.
func assertParsableSSE(t *testing.T, body string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("SSE body line %q has no field separator\nbody:\n%s", line, body)
		}
		switch name {
		case "event", "data", "id", "retry":
		default:
			t.Fatalf("SSE body line %q uses unknown field %q; clients discard it\nbody:\n%s", line, name, body)
		}
	}
}

func TestUnauthorizedRequestIsAccessLogged(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	s := New(config.Config{APIKey: "secret"}, modelsGateway{}, logger)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Request-ID", "req-unauthorized")

	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusUnauthorized, rr.Body.String())
	}
	entry := findLogEntry(t, buf.String(), "request completed")
	if entry["status"] != float64(http.StatusUnauthorized) {
		t.Fatalf("access log status = %#v, want 401", entry["status"])
	}
	if entry["path"] != "/v1/models" {
		t.Fatalf("access log path = %#v, want /v1/models", entry["path"])
	}
	if entry["request_id"] != "req-unauthorized" {
		t.Fatalf("access log request_id = %#v, want req-unauthorized", entry["request_id"])
	}
}

func TestPanickingHandlerIsAccessLoggedWithStatus500(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	s := New(config.Config{}, panicModelsGateway{}, logger)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("X-Request-ID", "req-panic")

	s.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusInternalServerError, rr.Body.String())
	}
	entry := findLogEntry(t, buf.String(), "request completed")
	if entry["status"] != float64(http.StatusInternalServerError) {
		t.Fatalf("access log status = %#v, want 500", entry["status"])
	}
	if entry["request_id"] != "req-panic" {
		t.Fatalf("access log request_id = %#v, want req-panic", entry["request_id"])
	}
}

func TestPanicAfterResponsesStreamCommitWritesTerminalSSEFailure(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	s := New(config.Config{}, &codexStreamGateway{}, logger)
	// The SSE writer emits one Write per frame, so panicking on the second
	// write leaves response.created already flushed to the client.
	rec := newPanicOnWriteRecorder(2)

	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi","stream":true}`)))

	body := rec.body.String()
	if !rec.panicked {
		t.Fatalf("test writer never panicked; body:\n%s", body)
	}
	assertParsableSSE(t, body)
	if !strings.Contains(body, "event: response.created\ndata: {") {
		t.Fatalf("stream was not committed before the panic; body:\n%s", body)
	}
	if !strings.Contains(body, "event: response.failed\ndata: {") {
		t.Fatalf("committed stream missing terminal response.failed frame; body:\n%s", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("committed stream did not end with the [DONE] sentinel; body:\n%s", body)
	}
	failed := sseEventPayload(t, body, "response.failed")
	if failed["status"] != "failed" {
		t.Fatalf("response.failed status = %#v, want failed", failed["status"])
	}
}

func TestPanicAfterChatStreamCommitWritesTerminalSSEFailure(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	s := New(config.Config{}, &streamChatGateway{}, logger)
	rec := newPanicOnWriteRecorder(2)

	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))

	body := rec.body.String()
	if !rec.panicked {
		t.Fatalf("test writer never panicked; body:\n%s", body)
	}
	assertParsableSSE(t, body)
	if !strings.Contains(body, `data: {"error":{`) {
		t.Fatalf("committed chat stream missing terminal error chunk; body:\n%s", body)
	}
	if !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("committed chat stream did not end with the [DONE] sentinel; body:\n%s", body)
	}
}

func TestRecoverMiddlewareRepanicsAbortHandler(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	rr := httptest.NewRecorder()
	h := recoverMiddleware(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	defer func() {
		v := recover()
		if v != http.ErrAbortHandler {
			t.Fatalf("recovered = %#v, want http.ErrAbortHandler", v)
		}
		if rr.Code != http.StatusOK || rr.Body.Len() != 0 {
			t.Fatalf("abort was converted into a response: %d %s", rr.Code, rr.Body.String())
		}
	}()

	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	t.Fatal("recoverMiddleware swallowed http.ErrAbortHandler")
}

// sseEventPayload decodes the data payload of the named SSE event.
func sseEventPayload(t *testing.T, body, event string) map[string]any {
	t.Helper()
	prefix := "event: " + event + "\ndata: "
	index := strings.Index(body, prefix)
	if index < 0 {
		t.Fatalf("event %q not found in body:\n%s", event, body)
	}
	rest := body[index+len(prefix):]
	end := strings.Index(rest, "\n\n")
	if end < 0 {
		t.Fatalf("event %q is not terminated in body:\n%s", event, body)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(rest[:end]), &payload); err != nil {
		t.Fatalf("decode %q payload: %v", event, err)
	}
	return payload
}
