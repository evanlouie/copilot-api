package httpapi

import (
	"context"
	"encoding/json"
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

// storingReasoningGateway models the durable half of the real gateway: it builds
// exactly one openai.Response per turn, hands that same object back to the
// caller and persists it. GET returns the persisted object. Reasoning emission
// is a presentation policy, so nothing the edge does for the wire may change
// what lands in (or comes back out of) this map.
type storingReasoningGateway struct {
	unimplementedGateway
	stored map[string]*openai.Response
}

func newStoringReasoningGateway() *storingReasoningGateway {
	return &storingReasoningGateway{stored: map[string]*openai.Response{}}
}

func (g *storingReasoningGateway) CreateResponse(_ context.Context, req copilotgw.ResponseRequest) (*copilotgw.ResponseResult, error) {
	resp := &openai.Response{
		ID:         req.ResponseID,
		Object:     openai.ObjectResponse,
		CreatedAt:  openai.UnixNow(),
		Status:     "completed",
		Model:      req.Model,
		OutputText: "answer",
		Output: []openai.ResponseOutputItem{
			{ID: "rs_rid-1", Type: "reasoning", Status: "completed", Summary: []openai.ResponseReasoningSummary{{Type: "summary_text", Text: "thinking"}}, EncryptedContent: "enc-blob"},
			{ID: "msg_1", Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: "answer"}}},
		},
		ParallelToolCalls: true,
		Store:             req.Store,
	}
	g.stored[resp.ID] = resp
	return &copilotgw.ResponseResult{Response: resp}, nil
}

func (g *storingReasoningGateway) GetResponse(_ context.Context, id string) (*openai.Response, error) {
	resp, ok := g.stored[id]
	if !ok {
		return nil, apierr.NotFound("response not found", "not_found")
	}
	return resp, nil
}

func hasReasoningItem(output []openai.ResponseOutputItem) bool {
	for _, item := range output {
		if item.Type == "reasoning" {
			return true
		}
	}
	return false
}

func postResponseForStore(t *testing.T, s *Server) openai.Response {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("POST /v1/responses status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp openai.Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("POST /v1/responses body: %v", err)
	}
	return resp
}

func getResponseForStore(t *testing.T, s *Server, id string) openai.Response {
	t.Helper()
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/responses/"+id, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/responses/%s status = %d, want 200: %s", id, w.Code, w.Body.String())
	}
	var resp openai.Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("GET /v1/responses/%s body: %v", id, err)
	}
	return resp
}

// TestReasoningEmissionOffDoesNotStripStoredReasoning pins the boundary:
// COPILOT_REASONING_EMISSION is a presentation knob, so a response created while
// it was "off" must still carry reasoning in the store, and reading it back with
// the knob at "both" must return that reasoning.
func TestReasoningEmissionOffDoesNotStripStoredReasoning(t *testing.T) {
	t.Parallel()
	gw := newStoringReasoningGateway()

	off := New(config.Config{ReasoningEmission: "off"}, gw, slog.Default())
	created := postResponseForStore(t, off)
	if hasReasoningItem(created.Output) {
		t.Fatalf("wire response must not carry reasoning when policy is off: %#v", created.Output)
	}

	stored, ok := gw.stored[created.ID]
	if !ok {
		t.Fatalf("gateway did not persist %s", created.ID)
	}
	if !hasReasoningItem(stored.Output) {
		t.Fatalf("stored record lost reasoning because the emission policy reached the gateway: %#v", stored.Output)
	}

	// A fresh server over the same store: flipping the knob back must restore
	// reasoning for records that already exist.
	both := New(config.Config{ReasoningEmission: "both"}, gw, slog.Default())
	fetched := getResponseForStore(t, both, created.ID)
	if !hasReasoningItem(fetched.Output) {
		t.Fatalf("GET with emission=both must return the stored reasoning item: %#v", fetched.Output)
	}
}

// TestReasoningEmissionOffFiltersStoredRecordOnRead is the other half of the
// same boundary: the read path still hides reasoning while the knob is off,
// and doing so must not mutate the gateway's memoised response object.
func TestReasoningEmissionOffFiltersStoredRecordOnRead(t *testing.T) {
	t.Parallel()
	gw := newStoringReasoningGateway()
	both := New(config.Config{ReasoningEmission: "both"}, gw, slog.Default())
	created := postResponseForStore(t, both)
	if !hasReasoningItem(created.Output) {
		t.Fatalf("wire response must carry reasoning when policy is both: %#v", created.Output)
	}

	off := New(config.Config{ReasoningEmission: "off"}, gw, slog.Default())
	fetched := getResponseForStore(t, off, created.ID)
	if hasReasoningItem(fetched.Output) {
		t.Fatalf("GET with emission=off must hide reasoning: %#v", fetched.Output)
	}
	if !hasReasoningItem(gw.stored[created.ID].Output) {
		t.Fatal("filtering the read path mutated the gateway's memoised response in place")
	}
}
