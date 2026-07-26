package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/openai"
)

func decodeErrorEnvelope(t *testing.T, status int, body string) openai.ErrorObject {
	t.Helper()
	var envelope openai.ErrorEnvelope
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("error body for status %d is not JSON (%v):\n%s", status, err, body)
	}
	return envelope.Error
}

// The official SDKs parse an error body as JSON unconditionally, so net/http's
// plain-text "404 page not found" reaches the caller as a parse error rather
// than as the 404 it is. Hitting /v1/embeddings on this proxy is the ordinary
// way to get there.
func TestUnroutedPathsReturnTheStandardErrorEnvelope(t *testing.T) {
	s := New(config.Config{}, modelsGateway{}, slog.Default())
	for _, tt := range []struct {
		name   string
		method string
		path   string
	}{
		{name: "unimplemented endpoint", method: http.MethodPost, path: "/v1/embeddings"},
		{name: "nested response path", method: http.MethodGet, path: "/v1/responses/resp_1/extra"},
		{name: "root", method: http.MethodGet, path: "/"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Fatalf("Content-Type = %q, want application/json", ct)
			}
			obj := decodeErrorEnvelope(t, rec.Code, rec.Body.String())
			// Matches the real API, which answers an unknown path with
			// `Invalid URL (POST /v1/embeddings)` and type invalid_request_error.
			if want := "Invalid URL (" + tt.method + " " + tt.path + ")"; obj.Message != want {
				t.Fatalf("message = %q, want %q", obj.Message, want)
			}
			if obj.Type != "invalid_request_error" {
				t.Fatalf("type = %q, want invalid_request_error", obj.Type)
			}
		})
	}
}

func TestWrongMethodReturnsTheStandardErrorEnvelope(t *testing.T) {
	s := New(config.Config{}, modelsGateway{}, slog.Default())
	for _, tt := range []struct {
		name    string
		method  string
		path    string
		allow   string
		message string
	}{
		// Byte-for-byte the real API's 405 body for this exact request.
		{name: "get on a post-only endpoint", method: http.MethodGet, path: "/v1/chat/completions", allow: "POST", message: "Only POST requests are accepted."},
		// net/http answers HEAD from a GET pattern, so it belongs in Allow.
		{name: "post on a get-only endpoint", method: http.MethodPost, path: "/v1/models", allow: "GET, HEAD", message: "Only GET, HEAD requests are accepted."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405: %s", rec.Code, rec.Body.String())
			}
			if allow := rec.Header().Get("Allow"); allow != tt.allow {
				t.Fatalf("Allow = %q, want %q", allow, tt.allow)
			}
			obj := decodeErrorEnvelope(t, rec.Code, rec.Body.String())
			// The real API answers a wrong method with `Only POST requests are
			// accepted.` and code method_not_supported.
			if obj.Message != tt.message {
				t.Fatalf("message = %q, want %q", obj.Message, tt.message)
			}
			if obj.Type != "invalid_request_error" || obj.Code != "method_not_supported" {
				t.Fatalf("error object = %#v", obj)
			}
		})
	}
}

// A 404 a handler produced on purpose already carries the authoritative body
// and must survive untouched.
func TestHandlerProducedNotFoundIsNotRewritten(t *testing.T) {
	s := New(config.Config{}, &statefulResponseGateway{}, slog.Default())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/responses/resp_missing", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	obj := decodeErrorEnvelope(t, rec.Code, rec.Body.String())
	if obj.Message != "response not found" || obj.Code != "not_found" {
		t.Fatalf("handler 404 was rewritten by the mux fallback: %#v", obj)
	}
}
