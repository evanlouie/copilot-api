package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

type metadataGateway struct {
	unimplementedGateway
	got    copilotgw.ResponseRequest
	stored map[string]*openai.Response
}

func (g *metadataGateway) CreateResponse(ctx context.Context, req copilotgw.ResponseRequest) (*copilotgw.ResponseResult, error) {
	g.got = req
	// Model what the real gateway does: the response it returns is the object it
	// persists, so metadata has to be on it for GET to find it later.
	resp := &openai.Response{
		ID: req.ResponseID, Object: openai.ObjectResponse, Status: "completed",
		Model: req.Model, Output: []openai.ResponseOutputItem{}, Metadata: req.Metadata,
	}
	if g.stored == nil {
		g.stored = map[string]*openai.Response{}
	}
	g.stored[req.ResponseID] = resp
	return &copilotgw.ResponseResult{Response: resp}, nil
}

func (g *metadataGateway) GetResponse(ctx context.Context, id string) (*openai.Response, error) {
	return g.stored[id], nil
}

// TestResponsesEchoesMetadata pins that the client's own tagging survives.
//
// metadata is round-trippable state on the real API - it is echoed on the
// response object and on GET /v1/responses/{id} - and this proxy used to accept
// it and drop it on the floor. That is the silent acceptance the validation
// policy rules out, and it degrades badly rather than gracefully: a client
// tagging responses with a trace id gets 200 OK and then finds the field gone
// on read, with nothing to say why.
func TestResponsesEchoesMetadata(t *testing.T) {
	t.Parallel()
	gw := &metadataGateway{}
	s := New(config.Config{}, gw, slog.Default())

	body := strings.NewReader(`{"model":"gpt-5","input":"hi","metadata":{"trace_id":"abc-123","tenant":"acme"}}`)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}

	if got := gw.got.Metadata["trace_id"]; got != "abc-123" {
		t.Fatalf("gateway received metadata %#v, want the client's tags", gw.got.Metadata)
	}

	var created openai.Response
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Metadata["trace_id"] != "abc-123" || created.Metadata["tenant"] != "acme" {
		t.Fatalf("response metadata = %#v, want it echoed", created.Metadata)
	}

	// The read-back path is the half that actually matters: it is why a client
	// sets metadata at all.
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/responses/"+created.ID, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", w.Code, w.Body.String())
	}
	var fetched openai.Response
	if err := json.Unmarshal(w.Body.Bytes(), &fetched); err != nil {
		t.Fatal(err)
	}
	if fetched.Metadata["trace_id"] != "abc-123" {
		t.Fatalf("GET metadata = %#v, want it echoed", fetched.Metadata)
	}
}

// TestResponsesRejectsOversizedMetadata keeps the acceptance bounded the way
// the real API bounds it.
func TestResponsesRejectsOversizedMetadata(t *testing.T) {
	t.Parallel()
	s := New(config.Config{}, &metadataGateway{}, slog.Default())
	body := strings.NewReader(`{"model":"gpt-5","input":"hi","metadata":{"k":"` + strings.Repeat("v", 513) + `"}}`)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "metadata") {
		t.Fatalf("error body does not name the field: %s", w.Body.String())
	}
}
