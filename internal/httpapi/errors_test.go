package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/evanlouie/copilot-api/internal/apierr"
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
