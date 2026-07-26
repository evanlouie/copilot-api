package openai

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/toolcatalog"
)

func TestNormalizeToolSearchOutputToolsRejectsRawPayloadTooLarge(t *testing.T) {
	t.Parallel()
	desc := strings.Repeat("x", toolcatalog.MaxLoadedRawToolsBytes)
	raw, _ := json.Marshal([]map[string]any{{"type": "function", "name": "lookup", "description": desc}})
	if _, err := NormalizeToolSearchOutputTools(raw, "input.0.tools"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("error = %v, want raw payload size rejection", err)
	}
}

func TestNormalizeToolSearchOutputToolsRejectsMixedFunctionShapes(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`[{"type":"function","name":"ignored_but_large","function":{"name":"lookup"}}]`)
	if _, err := NormalizeToolSearchOutputTools(raw, "input.0.tools"); err == nil || !strings.Contains(err.Error(), "cannot mix") {
		t.Fatalf("error = %v, want mixed function shape rejection", err)
	}
}

func TestNormalizeToolSearchOutputToolsRejectsHostedFields(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(`[{"type":"function","name":"lookup","server_url":"https://example.com","parameters":{"type":"object"}}]`)
	if _, err := NormalizeToolSearchOutputTools(raw, "input.0.tools"); err == nil || !strings.Contains(err.Error(), "unsupported field") {
		t.Fatalf("error = %v, want unsupported field rejection", err)
	}
}

func TestNormalizeToolSearchOutputToolsCanonicalKeyIgnoresJSONOrder(t *testing.T) {
	t.Parallel()
	a, err := NormalizeToolSearchOutputTools(json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]`), "input.0.tools")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NormalizeToolSearchOutputTools(json.RawMessage(`[{"parameters":{"properties":{"q":{"type":"string"}},"type":"object"},"name":"lookup","type":"function"}]`), "input.0.tools")
	if err != nil {
		t.Fatal(err)
	}
	ca, err := toolcatalog.NewToolCatalog(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := toolcatalog.NewToolCatalog(b)
	if err != nil {
		t.Fatal(err)
	}
	if ca.Key() != cb.Key() {
		t.Fatalf("catalog keys differ for reordered JSON: %q != %q", ca.Key(), cb.Key())
	}
}
