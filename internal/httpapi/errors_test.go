package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/openai"
)

// The domain layer only classifies failures by apierr.Kind; the HTTP status and
// the OpenAI error `type` are derived here and nowhere else. This pins the whole
// mapping so a taxonomy change cannot silently move a status on the wire.
func TestDomainErrorHTTPMapping(t *testing.T) {
	for _, test := range []struct {
		name    string
		err     error
		status  int
		errType string
		code    string
		param   string
	}{
		{"invalid_request", apierr.InvalidRequest("model is required", "model"), http.StatusBadRequest, "invalid_request_error", "", "model"},
		{"request_too_large", apierr.RequestTooLarge(), http.StatusRequestEntityTooLarge, "invalid_request_error", "request_too_large", "body"},
		{"unauthorized", apierr.Unauthorized("missing key"), http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", ""},
		{"not_found", apierr.NotFound("missing model", "model_not_found"), http.StatusNotFound, "invalid_request_error", "model_not_found", ""},
		{"previous_response_not_found", apierr.PreviousResponseNotFound("resp_1"), http.StatusBadRequest, "invalid_request_error", "previous_response_not_found", "previous_response_id"},
		{"upstream", apierr.Upstream("boom"), http.StatusBadGateway, "server_error", "upstream_error", ""},
		// A real 429 from OpenAI carries the exhausted dimension in `type` and
		// "rate_limit_exceeded" in `code`; see openai-node#168 and openai-python#2703.
		{"rate_limit", apierr.RateLimited("rate limit reached", 0), http.StatusTooManyRequests, "requests", "rate_limit_exceeded", ""},
		{"unavailable", apierr.Unavailable("gateway is shutting down"), http.StatusServiceUnavailable, "server_error", "service_unavailable", ""},
		{"timeout", apierr.Timeout(), http.StatusGatewayTimeout, "server_error", "request_timeout", ""},
		{"internal", apierr.Internal("internal server error"), http.StatusInternalServerError, "server_error", "internal_error", ""},
		{"unclassified", errors.New("/secret/path"), http.StatusInternalServerError, "server_error", "internal_error", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			WriteError(response, test.err)
			if response.Code != test.status {
				t.Fatalf("status = %d, want %d", response.Code, test.status)
			}
			var envelope openai.ErrorEnvelope
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Error.Type != test.errType || envelope.Error.Code != test.code || envelope.Error.Param != test.param {
				t.Fatalf("error = %#v, want type=%q code=%q param=%q", envelope.Error, test.errType, test.code, test.param)
			}
		})
	}
}

// Retry-After is what makes a 429 actionable: the official SDKs read it to
// schedule their backoff instead of guessing.
func TestRateLimitRetryAfterHeader(t *testing.T) {
	for _, test := range []struct {
		name       string
		retryAfter time.Duration
		want       string
	}{
		{name: "unknown wait omits the header", retryAfter: 0, want: ""},
		{name: "whole seconds", retryAfter: 30 * time.Second, want: "30"},
		// Rounded up, so the header never advertises a shorter wait than the
		// upstream asked for.
		{name: "sub-second rounds up", retryAfter: 1500 * time.Millisecond, want: "2"},
		{name: "under a second still waits", retryAfter: 10 * time.Millisecond, want: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			WriteError(response, apierr.RateLimited("rate limit reached", test.retryAfter))
			if response.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", response.Code)
			}
			if got := response.Header().Get("Retry-After"); got != test.want {
				t.Fatalf("Retry-After = %q, want %q", got, test.want)
			}
		})
	}
}

// An unclassified error must never reach the client verbatim.
func TestUnclassifiedErrorIsOpaque(t *testing.T) {
	response := httptest.NewRecorder()
	WriteError(response, errors.New("/secret/path"))
	if got := response.Body.String(); !json.Valid(response.Body.Bytes()) || got == "" {
		t.Fatalf("body = %q", got)
	}
	var envelope openai.ErrorEnvelope
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Message != "internal server error" {
		t.Fatalf("message = %q, want the generic message", envelope.Error.Message)
	}
}

// The taxonomy is only useful if a gateway failure actually carries it all the
// way to the wire, including the header the SDKs read.
func TestRateLimitFromTheGatewayReachesTheClientAsA429(t *testing.T) {
	s := New(config.Config{}, &errorResponseGateway{err: apierr.RateLimited("You have exceeded your premium request allowance.", 20*time.Second)}, slog.Default())
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi","stream":true}`)))

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "20" {
		t.Fatalf("Retry-After = %q, want 20", got)
	}
	var envelope openai.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "rate_limit_exceeded" || envelope.Error.Type != "requests" {
		t.Fatalf("error = %#v", envelope.Error)
	}
}
