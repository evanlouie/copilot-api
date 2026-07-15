package copilotgw

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	copilot "github.com/github/copilot-sdk/go"
)

func TestValidateResponseToolOutputsForBatchDetectsToolSearchInstallBoundary(t *testing.T) {
	broker := toolproxy.NewBroker(time.Minute)
	rt, err := toolproxy.NewResponseRequestTools(broker, []openai.NormalizedTool{{Kind: openai.ToolKindToolSearch, Name: "tool_search", Execution: "client"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	batch, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_search", Name: "tool_search", Arguments: map[string]any{"query": "agents"}}}, "resp_prev", "response", "gpt-test", make(chan toolproxy.TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	install, err := validateResponseToolOutputsForBatch(batch, map[string]openai.ResponseToolOutput{
		"call_search": {Kind: openai.ToolKindToolSearch, CallID: "call_search", Execution: "client", Status: "completed", Output: "loaded", LoadedTools: []openai.NormalizedTool{{Kind: openai.ToolKindFunction, Name: "loaded_tool"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !install {
		t.Fatal("expected install boundary for successful tool_search_output with loaded tools")
	}
}

func TestValidateResponseToolOutputsRejectsFailedToolSearchWithTools(t *testing.T) {
	broker := toolproxy.NewBroker(time.Minute)
	rt, err := toolproxy.NewResponseRequestTools(broker, []openai.NormalizedTool{{Kind: openai.ToolKindToolSearch, Name: "tool_search", Execution: "client"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	batch, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_search", Name: "tool_search", Arguments: map[string]any{"query": "agents"}}}, "resp_prev", "response", "gpt-test", make(chan toolproxy.TurnFinalResult, 1), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = validateResponseToolOutputsForBatch(batch, map[string]openai.ResponseToolOutput{
		"call_search": {Kind: openai.ToolKindToolSearch, CallID: "call_search", Execution: "client", Status: "failed", Output: "nope", LoadedTools: []openai.NormalizedTool{{Kind: openai.ToolKindFunction, Name: "loaded_tool"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot include tools") {
		t.Fatalf("error = %v, want failed-status loaded tool rejection", err)
	}
}

func TestMergeLoadedToolSearchOutputsUsesPreviousCatalogAndPersistsEvent(t *testing.T) {
	base, err := openai.NewToolCatalog([]openai.NormalizedTool{{Kind: openai.ToolKindToolSearch, Name: "tool_search", Execution: "client"}})
	if err != nil {
		t.Fatal(err)
	}
	previous := sessionstore.ResponseRecord{ID: "resp_prev", InstalledToolCatalog: storeToolCatalog(base.StoredDTO())}
	outputs := map[string]openai.ResponseToolOutput{
		"call_search": {Kind: openai.ToolKindToolSearch, CallID: "call_search", Execution: "client", Status: "completed", Tools: json.RawMessage(`[{"type":"namespace","name":"multi_agent_v1","tools":[{"name":"spawn_agent"}]}]`), LoadedTools: []openai.NormalizedTool{{Kind: openai.ToolKindNamespace, Name: "multi_agent_v1", Children: []openai.NormalizedTool{{Kind: openai.ToolKindFunction, Name: "spawn_agent"}}}}},
	}
	merge, err := mergeLoadedToolSearchOutputs(ResponseRequest{ResponseID: "resp_next", Model: "gpt-test"}, previous, outputs)
	if err != nil {
		t.Fatal(err)
	}
	if !merge.Changed || len(merge.Events) != 1 {
		t.Fatalf("merge = %#v, want changed with one event", merge)
	}
	flat := merge.Catalog.Flatten()
	if len(flat) != 2 || flat[1].Kind != openai.ToolKindNamespace || flat[1].Children[0].Name != "spawn_agent" {
		t.Fatalf("merged catalog = %#v", flat)
	}
	rt, err := toolproxy.NewResponseRequestTools(toolproxy.NewBroker(time.Minute), flat, false)
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(rt.AvailableTools(), "custom:multi_agent_v1__spawn_agent") {
		t.Fatalf("AvailableTools = %#v, want loaded namespace child", rt.AvailableTools())
	}
	none, err := toolproxy.NewResponseRequestTools(toolproxy.NewBroker(time.Minute), flat, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := none.AvailableTools(); len(got) != 1 || got[0] != toolproxy.NoToolsSentinel {
		t.Fatalf("tool_choice none AvailableTools = %#v, want sentinel only", got)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestResponseCatalogForContinuationMergesRequestToolsIntoStoredCatalog(t *testing.T) {
	base, err := openai.NewToolCatalog([]openai.NormalizedTool{
		{Kind: openai.ToolKindToolSearch, Name: "tool_search", Execution: "client"},
		{Kind: openai.ToolKindNamespace, Name: "multi_agent_v1", Children: []openai.NormalizedTool{{Kind: openai.ToolKindFunction, Name: "spawn_agent"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	previous := sessionstore.ResponseRecord{ID: "resp_prev", InstalledToolCatalog: storeToolCatalog(base.StoredDTO())}
	catalog, err := responseCatalogForRequest(ResponseRequest{ToolsSet: true, Tools: []openai.NormalizedTool{{Kind: openai.ToolKindToolSearch, Name: "tool_search", Execution: "client"}}}, &previous)
	if err != nil {
		t.Fatal(err)
	}
	flat := catalog.Flatten()
	if len(flat) != 2 || flat[1].Kind != openai.ToolKindNamespace || flat[1].Children[0].Name != "spawn_agent" {
		t.Fatalf("catalog after request tools = %#v, want stored dynamic namespace preserved", flat)
	}
}

func TestActiveResponseToolOutputsFromRecordRejectsSpoofedLoadedToolCallID(t *testing.T) {
	record := sessionstore.ResponseRecord{ID: "resp_prev", Output: storeOutputItems([]openai.ResponseOutputItem{{Type: "tool_search_call", CallID: "call_real", Execution: "client"}})}
	_, err := activeResponseToolOutputsFromRecord(record, map[string]openai.ResponseToolOutput{
		"call_real":  {Kind: openai.ToolKindToolSearch, CallID: "call_real", Execution: "client", Output: "none"},
		"call_spoof": {Kind: openai.ToolKindToolSearch, CallID: "call_spoof", Execution: "client", LoadedTools: []openai.NormalizedTool{{Kind: openai.ToolKindFunction, Name: "evil"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "does not belong to previous_response_id") {
		t.Fatalf("error = %v, want spoofed call_id rejection", err)
	}
}

func TestActiveResponseToolOutputsFromRecordRejectsLoadedToolsForFunctionCall(t *testing.T) {
	record := sessionstore.ResponseRecord{ID: "resp_prev", Output: storeOutputItems([]openai.ResponseOutputItem{{Type: "function_call", CallID: "call_lookup", Name: "lookup"}})}
	_, err := activeResponseToolOutputsFromRecord(record, map[string]openai.ResponseToolOutput{
		"call_lookup": {Kind: openai.ToolKindFunction, CallID: "call_lookup", LoadedTools: []openai.NormalizedTool{{Kind: openai.ToolKindFunction, Name: "evil"}}},
	})
	if err == nil || !strings.Contains(err.Error(), "only tool_search_output") {
		t.Fatalf("error = %v, want non-search loaded tool rejection", err)
	}
}

func TestMergeLoadedToolSearchOutputsRequiresCatalogForMigratedRecords(t *testing.T) {
	outputs := map[string]openai.ResponseToolOutput{
		"call_search": {Kind: openai.ToolKindToolSearch, CallID: "call_search", Execution: "client", Status: "completed", LoadedTools: []openai.NormalizedTool{{Kind: openai.ToolKindFunction, Name: "loaded_tool"}}},
	}
	_, err := mergeLoadedToolSearchOutputs(ResponseRequest{ResponseID: "resp_next", Model: "gpt-test"}, sessionstore.ResponseRecord{ID: "resp_prev"}, outputs)
	if err == nil || !strings.Contains(err.Error(), "does not contain an installed tool catalog") {
		t.Fatalf("error = %v, want migrated-record catalog error", err)
	}
}
