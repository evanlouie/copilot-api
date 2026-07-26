package copilotgw

import (
	"sort"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
)

type responseCatalogMergeResult struct {
	Catalog toolcatalog.ToolCatalog
	Events  []toolcatalog.StoredLoadedToolEvent
	Changed bool
}

func responseCatalogForRequest(req ResponseRequest, previous *sessionstore.ResponseRecord) (toolcatalog.ToolCatalog, error) {
	if previous != nil && previous.InstalledToolCatalog != nil {
		catalog, _, err := toolcatalog.ToolCatalogFromStored(previous.InstalledToolCatalog)
		if err != nil {
			return toolcatalog.ToolCatalog{}, err
		}
		if req.ToolsSet {
			return catalog.MergeRequestTools(req.Tools)
		}
		return catalog, nil
	}
	return toolcatalog.NewToolCatalog(req.Tools)
}

func responseCatalogDTOForRequest(req ResponseRequest, previous *sessionstore.ResponseRecord) (*toolcatalog.StoredToolCatalog, error) {
	catalog, err := responseCatalogForRequest(req, previous)
	if err != nil {
		return nil, err
	}
	return catalog.StoredDTO(), nil
}

func mergeLoadedToolSearchOutputs(req ResponseRequest, previous sessionstore.ResponseRecord, outputs map[string]toolcatalog.ResponseToolOutput) (responseCatalogMergeResult, error) {
	for _, output := range outputs {
		if output.Kind == toolcatalog.ToolKindToolSearch && !toolSearchOutputStatusInstallable(output.Status) && len(output.LoadedTools) > 0 {
			return responseCatalogMergeResult{}, apierr.InvalidRequest("tool_search_output with failed, incomplete, cancelled, or unknown status cannot include tools", "input")
		}
	}
	if previous.InstalledToolCatalog == nil && !req.ToolsSet && responseOutputsContainLoadedTools(outputs) {
		return responseCatalogMergeResult{}, apierr.InvalidRequest("previous response does not contain an installed tool catalog; resubmit tools to install tool_search_output.tools", "previous_response_id")
	}
	catalog, err := responseCatalogForRequest(req, &previous)
	if err != nil {
		return responseCatalogMergeResult{}, err
	}
	ids := make([]string, 0, len(outputs))
	for id, output := range outputs {
		if output.Kind == toolcatalog.ToolKindToolSearch && len(output.LoadedTools) > 0 && toolSearchOutputStatusInstallable(output.Status) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	result := responseCatalogMergeResult{Catalog: catalog}
	for _, id := range ids {
		output := outputs[id]
		merged, err := result.Catalog.MergeLoaded(id, output.LoadedTools)
		if err != nil {
			return responseCatalogMergeResult{}, err
		}
		if merged.Key() != result.Catalog.Key() {
			result.Changed = true
		}
		result.Catalog = merged
		result.Events = append(result.Events, toolcatalog.StoredLoadedToolEventFromLoaded(toolcatalog.LoadedToolEvent{SourceCallID: id, ResponseID: req.ResponseID, Status: output.Status, Execution: output.Execution, RawTools: output.Tools, LoadedTools: output.LoadedTools}))
	}
	return result, nil
}

func responseOutputsContainLoadedTools(outputs map[string]toolcatalog.ResponseToolOutput) bool {
	for _, output := range outputs {
		if output.Kind == toolcatalog.ToolKindToolSearch && len(output.LoadedTools) > 0 && toolSearchOutputStatusInstallable(output.Status) {
			return true
		}
	}
	return false
}

func activeResponseToolOutputsFromRecord(record sessionstore.ResponseRecord, outputs map[string]toolcatalog.ResponseToolOutput) (map[string]toolcatalog.ResponseToolOutput, error) {
	expected := map[string]openai.ResponseOutputItem{}
	for _, item := range record.Output {
		if item.CallID == "" {
			continue
		}
		switch item.Type {
		case "function_call", "custom_tool_call", "tool_search_call":
			expected[item.CallID] = item
		}
	}
	if len(expected) == 0 {
		for _, output := range outputs {
			if len(output.LoadedTools) > 0 {
				return nil, apierr.InvalidRequest("tool output call_id does not belong to previous_response_id", "input")
			}
		}
		return nil, apierr.InvalidRequest("previous response has no pending tool calls", "previous_response_id")
	}
	active := make(map[string]toolcatalog.ResponseToolOutput, len(expected))
	for id, output := range outputs {
		previous, ok := expected[id]
		if !ok {
			if len(output.LoadedTools) > 0 {
				return nil, apierr.InvalidRequest("tool output call_id does not belong to previous_response_id", "input")
			}
			continue
		}
		if err := validateResponseToolOutputForItem(previous, output); err != nil {
			return nil, err
		}
		active[id] = output
	}
	for id := range expected {
		if _, ok := active[id]; !ok {
			return nil, apierr.InvalidRequest("expected exactly one output for each pending tool call", "input")
		}
	}
	return active, nil
}

func validateResponseToolOutputForItem(previous openai.ResponseOutputItem, output toolcatalog.ResponseToolOutput) error {
	expectedKind := toolcatalog.ToolKindFunction
	switch previous.Type {
	case "custom_tool_call":
		expectedKind = toolcatalog.ToolKindCustom
	case "tool_search_call":
		expectedKind = toolcatalog.ToolKindToolSearch
	}
	if output.Kind != "" && output.Kind != expectedKind {
		return apierr.InvalidRequest(string(output.Kind)+" output does not match previous "+string(expectedKind)+" call", "input")
	}
	if expectedKind == toolcatalog.ToolKindCustom && output.Name != "" && previous.Name != "" && output.Name != previous.Name {
		return apierr.InvalidRequest("custom_tool_call_output name does not match previous custom tool", "input")
	}
	if expectedKind == toolcatalog.ToolKindToolSearch {
		if previous.Execution != "" && previous.Execution != "client" {
			return apierr.InvalidRequest("previous tool_search_call execution is not client", "input")
		}
		if output.Execution != "" && output.Execution != "client" {
			return apierr.InvalidRequest("tool_search_output execution must be client", "input")
		}
		if !toolSearchOutputStatusInstallable(output.Status) && len(output.LoadedTools) > 0 {
			return apierr.InvalidRequest("tool_search_output with failed, incomplete, cancelled, or unknown status cannot include tools", "input")
		}
		return nil
	}
	if len(output.LoadedTools) > 0 {
		return apierr.InvalidRequest("only tool_search_output can include loadable tools", "input")
	}
	return nil
}

func validateResponseToolOutputsForBatch(batch *toolproxy.Batch, outputs map[string]toolcatalog.ResponseToolOutput) (bool, error) {
	calls := batch.CapturedCalls()
	if len(outputs) != len(calls) {
		return false, apierr.InvalidRequest("expected exactly one output for each pending tool call", "input")
	}
	installBoundary := false
	for id, output := range outputs {
		call, ok := batch.CapturedCall(id)
		if !ok {
			return false, apierr.InvalidRequest("unknown tool call output call_id", "input")
		}
		if output.Kind != "" && call.Kind != "" && output.Kind != call.Kind {
			return false, apierr.InvalidRequest(string(output.Kind)+" output does not match pending "+string(call.Kind)+" call", "input")
		}
		if call.Kind == toolcatalog.ToolKindCustom && output.Name != "" && call.ResponseName != "" && output.Name != call.ResponseName {
			return false, apierr.InvalidRequest("custom_tool_call_output name does not match pending custom tool", "input")
		}
		if output.Kind != toolcatalog.ToolKindToolSearch {
			continue
		}
		if call.Kind != toolcatalog.ToolKindToolSearch {
			return false, apierr.InvalidRequest("tool_search_output does not match pending tool_search call", "input")
		}
		if call.Execution != "" && call.Execution != "client" {
			return false, apierr.InvalidRequest("pending tool_search call execution is not client", "input")
		}
		if output.Execution != "" && output.Execution != "client" {
			return false, apierr.InvalidRequest("tool_search_output execution must be client", "input")
		}
		if !toolSearchOutputStatusInstallable(output.Status) {
			if len(output.LoadedTools) > 0 {
				return false, apierr.InvalidRequest("tool_search_output with failed, incomplete, cancelled, or unknown status cannot include tools", "input")
			}
			continue
		}
		if len(output.LoadedTools) > 0 {
			installBoundary = true
		}
	}
	return installBoundary, nil
}

func toolSearchOutputStatusInstallable(status string) bool {
	switch status {
	case "", "success", "completed":
		return true
	default:
		return false
	}
}
