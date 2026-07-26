package toolcatalog

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

func TestNewToolCatalogRejectsOversizedInitialCatalog(t *testing.T) {
	_, err := NewToolCatalog([]NormalizedTool{{Kind: ToolKindFunction, Name: "lookup", Description: strings.Repeat("x", MaxLoadedCatalogBytes)}})
	if err == nil || !strings.Contains(err.Error(), "catalog is too large") {
		t.Fatalf("error = %v, want initial catalog size rejection", err)
	}
}

func TestNewToolCatalogRejectsEmptyNamespace(t *testing.T) {
	_, err := NewToolCatalog([]NormalizedTool{{Kind: ToolKindNamespace, Name: "empty"}})
	if err == nil || !strings.Contains(err.Error(), "at least one child") {
		t.Fatalf("error = %v, want empty namespace rejection", err)
	}
}

func TestNewToolCatalogRejectsDuplicateNamespaces(t *testing.T) {
	_, err := NewToolCatalog([]NormalizedTool{
		{Kind: ToolKindNamespace, Name: "duplicate", Children: []NormalizedTool{{Kind: ToolKindFunction, Name: "one"}}},
		{Kind: ToolKindNamespace, Name: "duplicate", Children: []NormalizedTool{{Kind: ToolKindFunction, Name: "two"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate Responses namespace") {
		t.Fatalf("error = %v, want duplicate namespace rejection", err)
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
