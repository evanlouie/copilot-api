package copilotgw

import (
	"sort"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
)

type responseCatalogMergeResult struct {
	Catalog openai.ToolCatalog
	Events  []openai.StoredLoadedToolEvent
	Changed bool
}

func responseCatalogForRequest(req ResponseRequest, previous *sessionstore.ResponseRecord) (openai.ToolCatalog, error) {
	if previous != nil && !req.ToolsSet && previous.InstalledToolCatalog != nil {
		catalog, _, err := openai.ToolCatalogFromStored(previous.InstalledToolCatalog)
		return catalog, err
	}
	return openai.NewToolCatalog(req.Tools)
}

func responseCatalogDTOForRequest(req ResponseRequest, previous *sessionstore.ResponseRecord) (*openai.StoredToolCatalog, error) {
	catalog, err := responseCatalogForRequest(req, previous)
	if err != nil {
		return nil, err
	}
	return catalog.StoredDTO(), nil
}

func mergeLoadedToolSearchOutputs(req ResponseRequest, previous sessionstore.ResponseRecord, outputs map[string]openai.ResponseToolOutput) (responseCatalogMergeResult, error) {
	for _, output := range outputs {
		if output.Kind == openai.ToolKindToolSearch && !toolSearchOutputStatusInstallable(output.Status) && len(output.LoadedTools) > 0 {
			return responseCatalogMergeResult{}, openai.InvalidRequest("tool_search_output with failed, incomplete, cancelled, or unknown status cannot include tools", "input")
		}
	}
	if previous.InstalledToolCatalog == nil && !req.ToolsSet && responseOutputsContainLoadedTools(outputs) {
		return responseCatalogMergeResult{}, openai.InvalidRequest("previous response does not contain an installed tool catalog; resubmit tools to install tool_search_output.tools", "previous_response_id")
	}
	catalog, err := responseCatalogForRequest(req, &previous)
	if err != nil {
		return responseCatalogMergeResult{}, err
	}
	ids := make([]string, 0, len(outputs))
	for id, output := range outputs {
		if output.Kind == openai.ToolKindToolSearch && len(output.LoadedTools) > 0 && toolSearchOutputStatusInstallable(output.Status) {
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
		result.Events = append(result.Events, openai.StoredLoadedToolEventFromLoaded(openai.LoadedToolEvent{SourceCallID: id, ResponseID: req.ResponseID, Status: output.Status, Execution: output.Execution, RawTools: output.Tools, LoadedTools: output.LoadedTools}))
	}
	return result, nil
}

func responseOutputsContainLoadedTools(outputs map[string]openai.ResponseToolOutput) bool {
	for _, output := range outputs {
		if output.Kind == openai.ToolKindToolSearch && len(output.LoadedTools) > 0 && toolSearchOutputStatusInstallable(output.Status) {
			return true
		}
	}
	return false
}

func validateResponseToolOutputsForBatch(batch *toolproxy.Batch, outputs map[string]openai.ResponseToolOutput) (bool, error) {
	calls := batch.CapturedCalls()
	if len(outputs) != len(calls) {
		return false, openai.InvalidRequest("expected exactly one output for each pending tool call", "input")
	}
	installBoundary := false
	for id, output := range outputs {
		call, ok := batch.CapturedCall(id)
		if !ok {
			return false, openai.InvalidRequest("unknown tool call output call_id", "input")
		}
		if output.Kind != "" && call.Kind != "" && output.Kind != call.Kind {
			return false, openai.InvalidRequest(string(output.Kind)+" output does not match pending "+string(call.Kind)+" call", "input")
		}
		if call.Kind == openai.ToolKindCustom && output.Name != "" && call.ResponseName != "" && output.Name != call.ResponseName {
			return false, openai.InvalidRequest("custom_tool_call_output name does not match pending custom tool", "input")
		}
		if output.Kind != openai.ToolKindToolSearch {
			continue
		}
		if call.Kind != openai.ToolKindToolSearch {
			return false, openai.InvalidRequest("tool_search_output does not match pending tool_search call", "input")
		}
		if call.Execution != "" && call.Execution != "client" {
			return false, openai.InvalidRequest("pending tool_search call execution is not client", "input")
		}
		if output.Execution != "" && output.Execution != "client" {
			return false, openai.InvalidRequest("tool_search_output execution must be client", "input")
		}
		if !toolSearchOutputStatusInstallable(output.Status) {
			if len(output.LoadedTools) > 0 {
				return false, openai.InvalidRequest("tool_search_output with failed, incomplete, cancelled, or unknown status cannot include tools", "input")
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
