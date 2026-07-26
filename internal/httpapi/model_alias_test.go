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

type effortAliasGateway struct {
	unimplementedGateway
	models []copilotgw.Model
}

func (g *effortAliasGateway) ListModels(ctx context.Context) ([]copilotgw.Model, error) {
	return g.models, nil
}

// TestEveryPublishedModelAliasParsesBack closes the loop between the two places
// that have to agree about what a reasoning effort is.
//
// GET /v1/models mints a "<model>:<effort>" alias for every entry in the
// catalog's SupportedReasoningEfforts, which is an unconstrained string list,
// while ParseModelSelector only splits a suffix that names a canonical effort.
// When those disagree this endpoint advertises an id that POST answers with
// 404 model_not_found - for a name the proxy itself invented.
func TestEveryPublishedModelAliasParsesBack(t *testing.T) {
	t.Parallel()
	gw := &effortAliasGateway{models: []copilotgw.Model{{
		ID: "gpt-5.1-codex",
		// "max" is real - the SDK documents it - and "banana" stands in for
		// anything a future catalog might add that this proxy does not know.
		SupportedReasoningEfforts: []string{"low", "high", "xhigh", "max", "banana"},
		DefaultReasoningEffort:    "medium",
	}}}
	s := New(config.Config{}, gw, slog.Default())
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	var list openai.ModelList
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}

	var aliases int
	for _, m := range list.Data {
		if !strings.Contains(m.ID, ":") {
			continue
		}
		aliases++
		selector, err := openai.ParseModelSelector(m.ID)
		if err != nil {
			t.Fatalf("published alias %q does not parse: %v", m.ID, err)
		}
		if !selector.HasEffort {
			t.Fatalf("published alias %q parsed as the whole model id %q, so a request for it would 404", m.ID, selector.Model)
		}
		if selector.Model != "gpt-5.1-codex" {
			t.Fatalf("alias %q resolved to model %q", m.ID, selector.Model)
		}
		if err := openai.ValidateReasoningEffort(selector.ReasoningEffort, "reasoning_effort"); err != nil {
			t.Fatalf("published alias %q carries an effort POST rejects: %v", m.ID, err)
		}
	}
	if aliases == 0 {
		t.Fatal("no aliases were published, so this test proved nothing")
	}
	for _, m := range list.Data {
		if strings.HasSuffix(m.ID, ":banana") {
			t.Fatalf("published an alias for an effort outside the canonical set: %q", m.ID)
		}
	}
}
