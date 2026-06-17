package openai

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestToolCatalogMergeLoadedNamespacePersistsCanonicalCatalog(t *testing.T) {
	base, err := NewToolCatalog([]NormalizedTool{{Kind: ToolKindToolSearch, Name: "tool_search", Execution: "client"}})
	if err != nil {
		t.Fatal(err)
	}
	loaded := []NormalizedTool{{Kind: ToolKindNamespace, Name: "multi_agent_v1", Description: "agents", Children: []NormalizedTool{{Kind: ToolKindFunction, Name: "spawn_agent", Description: "spawn", Parameters: json.RawMessage(`{"properties":{"task":{"type":"string"}},"type":"object"}`), Raw: json.RawMessage(`{"sensitive":"not persisted"}`)}}}}
	merged, err := base.MergeLoaded("call_search", loaded)
	if err != nil {
		t.Fatal(err)
	}
	if merged.Key() == base.Key() {
		t.Fatal("catalog key did not change after loading namespace")
	}
	stored := merged.StoredDTO()
	b, _ := json.Marshal(stored)
	if strings.Contains(string(b), "sensitive") {
		t.Fatalf("stored catalog leaked raw tool JSON: %s", b)
	}
	rehydrated, ok, err := ToolCatalogFromStored(stored)
	if err != nil || !ok {
		t.Fatalf("ToolCatalogFromStored = ok %v err %v", ok, err)
	}
	if rehydrated.Key() != merged.Key() {
		t.Fatalf("rehydrated key = %q, want %q", rehydrated.Key(), merged.Key())
	}
	flat := rehydrated.Flatten()
	if len(flat) != 2 || flat[1].Kind != ToolKindNamespace || flat[1].Children[0].Namespace != "multi_agent_v1" {
		t.Fatalf("rehydrated tools = %#v, want namespace child with namespace", flat)
	}
}

func TestToolCatalogMergeLoadedRejectsConflictingDefinition(t *testing.T) {
	base, err := NewToolCatalog([]NormalizedTool{{Kind: ToolKindFunction, Name: "lookup", Description: "old"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.MergeLoaded("call_search", []NormalizedTool{{Kind: ToolKindFunction, Name: "lookup", Description: "new"}}); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("error = %v, want conflict", err)
	}
	if _, err := base.MergeLoaded("call_search", []NormalizedTool{{Kind: ToolKindFunction, Name: "lookup", Description: "old"}}); err != nil {
		t.Fatalf("identical duplicate should be idempotent: %v", err)
	}
}

func TestToolCatalogMergeLoadedRejectsCumulativeToolCountLimit(t *testing.T) {
	baseTools := make([]NormalizedTool, 0, MaxInstalledToolCount)
	for i := 0; i < MaxInstalledToolCount; i++ {
		baseTools = append(baseTools, NormalizedTool{Kind: ToolKindFunction, Name: fmt.Sprintf("base_%03d", i)})
	}
	base, err := NewToolCatalog(baseTools)
	if err != nil {
		t.Fatal(err)
	}
	_, err = base.MergeLoaded("call_search", []NormalizedTool{{Kind: ToolKindFunction, Name: "one_more"}})
	if err == nil || !strings.Contains(err.Error(), "too many tools") {
		t.Fatalf("error = %v, want cumulative tool count rejection", err)
	}
}

func TestNormalizeToolSearchOutputToolsRejectsRawPayloadTooLarge(t *testing.T) {
	desc := strings.Repeat("x", MaxLoadedRawToolsBytes)
	raw, _ := json.Marshal([]map[string]any{{"type": "function", "name": "lookup", "description": desc}})
	if _, err := NormalizeToolSearchOutputTools(raw, "input.0.tools"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("error = %v, want raw payload size rejection", err)
	}
}

func TestNormalizeToolSearchOutputToolsRejectsMixedFunctionShapes(t *testing.T) {
	raw := json.RawMessage(`[{"type":"function","name":"ignored_but_large","function":{"name":"lookup"}}]`)
	if _, err := NormalizeToolSearchOutputTools(raw, "input.0.tools"); err == nil || !strings.Contains(err.Error(), "cannot mix") {
		t.Fatalf("error = %v, want mixed function shape rejection", err)
	}
}

func TestNormalizeToolSearchOutputToolsRejectsHostedFields(t *testing.T) {
	raw := json.RawMessage(`[{"type":"function","name":"lookup","server_url":"https://example.com","parameters":{"type":"object"}}]`)
	if _, err := NormalizeToolSearchOutputTools(raw, "input.0.tools"); err == nil || !strings.Contains(err.Error(), "unsupported field") {
		t.Fatalf("error = %v, want unsupported field rejection", err)
	}
}

func TestNormalizeToolSearchOutputToolsCanonicalKeyIgnoresJSONOrder(t *testing.T) {
	a, err := NormalizeToolSearchOutputTools(json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]`), "input.0.tools")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NormalizeToolSearchOutputTools(json.RawMessage(`[{"parameters":{"properties":{"q":{"type":"string"}},"type":"object"},"name":"lookup","type":"function"}]`), "input.0.tools")
	if err != nil {
		t.Fatal(err)
	}
	ca, err := NewToolCatalog(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := NewToolCatalog(b)
	if err != nil {
		t.Fatal(err)
	}
	if ca.Key() != cb.Key() {
		t.Fatalf("catalog keys differ for reordered JSON: %q != %q", ca.Key(), cb.Key())
	}
}
