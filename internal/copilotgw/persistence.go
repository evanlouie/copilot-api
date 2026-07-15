package copilotgw

import (
	"encoding/json"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
)

func storeOutputItems(items []openai.ResponseOutputItem) []sessionstore.ResponseOutputItem {
	out := make([]sessionstore.ResponseOutputItem, len(items))
	for i, item := range items {
		out[i] = sessionstore.ResponseOutputItem{
			ID: item.ID, Type: item.Type, Status: item.Status, Role: item.Role,
			EncryptedContent: item.EncryptedContent, CallID: item.CallID, Name: item.Name,
			Namespace: item.Namespace, Arguments: item.Arguments,
			ArgumentsJSON: cloneRaw(item.ArgumentsJSON), Input: item.Input,
			Execution: item.Execution, Output: cloneRaw(item.Output),
		}
		if item.Content != nil {
			out[i].Content = make([]sessionstore.ResponseText, len(item.Content))
			for j, content := range item.Content {
				out[i].Content[j] = sessionstore.ResponseText{Type: content.Type, Text: content.Text, Annotations: append([]any(nil), content.Annotations...)}
			}
		}
		if item.Summary != nil {
			out[i].Summary = make([]sessionstore.ResponseReasoningSummary, len(item.Summary))
			for j, summary := range item.Summary {
				out[i].Summary[j] = sessionstore.ResponseReasoningSummary{Type: summary.Type, Text: summary.Text}
			}
		}
	}
	return out
}

func wireOutputItems(items []sessionstore.ResponseOutputItem) []openai.ResponseOutputItem {
	out := make([]openai.ResponseOutputItem, len(items))
	for i, item := range items {
		out[i] = openai.ResponseOutputItem{
			ID: item.ID, Type: item.Type, Status: item.Status, Role: item.Role,
			EncryptedContent: item.EncryptedContent, CallID: item.CallID, Name: item.Name,
			Namespace: item.Namespace, Arguments: item.Arguments,
			ArgumentsJSON: cloneRaw(item.ArgumentsJSON), Input: item.Input,
			Execution: item.Execution, Output: cloneRaw(item.Output),
		}
		if item.Content != nil {
			out[i].Content = make([]openai.ResponseText, len(item.Content))
			for j, content := range item.Content {
				out[i].Content[j] = openai.ResponseText{Type: content.Type, Text: content.Text, Annotations: append([]any(nil), content.Annotations...)}
			}
		}
		if item.Summary != nil {
			out[i].Summary = make([]openai.ResponseReasoningSummary, len(item.Summary))
			for j, summary := range item.Summary {
				out[i].Summary[j] = openai.ResponseReasoningSummary{Type: summary.Type, Text: summary.Text}
			}
		}
	}
	return out
}

func storeUsage(usage *openai.ResponseUsage) *sessionstore.ResponseUsage {
	if usage == nil {
		return nil
	}
	out := &sessionstore.ResponseUsage{InputTokens: cloneInt64Ptr(usage.InputTokens), OutputTokens: cloneInt64Ptr(usage.OutputTokens), TotalTokens: cloneInt64Ptr(usage.TotalTokens)}
	if usage.InputTokensDetails != nil {
		out.InputTokensDetails = &sessionstore.ResponseInputTokensDetails{CachedTokens: cloneInt64Ptr(usage.InputTokensDetails.CachedTokens)}
	}
	if usage.OutputTokensDetails != nil {
		out.OutputTokensDetails = &sessionstore.ResponseOutputTokensDetails{ReasoningTokens: cloneInt64Ptr(usage.OutputTokensDetails.ReasoningTokens)}
	}
	return out
}

func wireUsage(usage *sessionstore.ResponseUsage) *openai.ResponseUsage {
	if usage == nil {
		return nil
	}
	out := &openai.ResponseUsage{InputTokens: cloneInt64Ptr(usage.InputTokens), OutputTokens: cloneInt64Ptr(usage.OutputTokens), TotalTokens: cloneInt64Ptr(usage.TotalTokens)}
	if usage.InputTokensDetails != nil {
		out.InputTokensDetails = &openai.ResponseInputTokensDetails{CachedTokens: cloneInt64Ptr(usage.InputTokensDetails.CachedTokens)}
	}
	if usage.OutputTokensDetails != nil {
		out.OutputTokensDetails = &openai.ResponseOutputTokensDetails{ReasoningTokens: cloneInt64Ptr(usage.OutputTokensDetails.ReasoningTokens)}
	}
	return out
}

func storeToolCatalog(catalog *openai.StoredToolCatalog) *sessionstore.StoredToolCatalog {
	if catalog == nil {
		return nil
	}
	return &sessionstore.StoredToolCatalog{SchemaVersion: catalog.SchemaVersion, CatalogKey: catalog.CatalogKey, KnownEmpty: catalog.KnownEmpty, Tools: storeToolSpecs(catalog.Tools)}
}

func wireToolCatalog(catalog *sessionstore.StoredToolCatalog) *openai.StoredToolCatalog {
	if catalog == nil {
		return nil
	}
	return &openai.StoredToolCatalog{SchemaVersion: catalog.SchemaVersion, CatalogKey: catalog.CatalogKey, KnownEmpty: catalog.KnownEmpty, Tools: wireToolSpecs(catalog.Tools)}
}

func storeToolSpecs(specs []openai.StoredToolSpec) []sessionstore.StoredToolSpec {
	if specs == nil {
		return nil
	}
	out := make([]sessionstore.StoredToolSpec, len(specs))
	for i, spec := range specs {
		out[i] = sessionstore.StoredToolSpec{Type: string(spec.Type), Name: spec.Name, Namespace: spec.Namespace, Description: spec.Description, Parameters: cloneRaw(spec.Parameters), Format: cloneRaw(spec.Format), Execution: spec.Execution, Strict: cloneBoolPtr(spec.Strict), DeferLoading: cloneBoolPtr(spec.DeferLoading), Tools: storeToolSpecs(spec.Tools)}
	}
	return out
}

func wireToolSpecs(specs []sessionstore.StoredToolSpec) []openai.StoredToolSpec {
	if specs == nil {
		return nil
	}
	out := make([]openai.StoredToolSpec, len(specs))
	for i, spec := range specs {
		out[i] = openai.StoredToolSpec{Type: openai.ResponsesToolKind(spec.Type), Name: spec.Name, Namespace: spec.Namespace, Description: spec.Description, Parameters: cloneRaw(spec.Parameters), Format: cloneRaw(spec.Format), Execution: spec.Execution, Strict: cloneBoolPtr(spec.Strict), DeferLoading: cloneBoolPtr(spec.DeferLoading), Tools: wireToolSpecs(spec.Tools)}
	}
	return out
}

func storeLoadedToolEvents(events []openai.StoredLoadedToolEvent) []sessionstore.StoredLoadedToolEvent {
	if events == nil {
		return nil
	}
	out := make([]sessionstore.StoredLoadedToolEvent, len(events))
	for i, event := range events {
		out[i] = sessionstore.StoredLoadedToolEvent{SourceCallID: event.SourceCallID, ResponseID: event.ResponseID, Status: event.Status, Execution: event.Execution, RawTools: cloneRaw(event.RawTools), LoadedTools: storeToolSpecs(event.LoadedTools)}
	}
	return out
}

func wireLoadedToolEvents(events []sessionstore.StoredLoadedToolEvent) []openai.StoredLoadedToolEvent {
	if events == nil {
		return nil
	}
	out := make([]openai.StoredLoadedToolEvent, len(events))
	for i, event := range events {
		out[i] = openai.StoredLoadedToolEvent{SourceCallID: event.SourceCallID, ResponseID: event.ResponseID, Status: event.Status, Execution: event.Execution, RawTools: cloneRaw(event.RawTools), LoadedTools: wireToolSpecs(event.LoadedTools)}
	}
	return out
}

func storeToolOutputs(outputs []openai.StoredToolOutput) []sessionstore.StoredToolOutput {
	if outputs == nil {
		return nil
	}
	out := make([]sessionstore.StoredToolOutput, len(outputs))
	for i, output := range outputs {
		out[i] = sessionstore.StoredToolOutput{Type: output.Type, CallID: output.CallID, Name: output.Name, Output: output.Output, Status: output.Status, Execution: output.Execution, Tools: cloneRaw(output.Tools)}
	}
	return out
}

func wireToolOutputs(outputs []sessionstore.StoredToolOutput) []openai.StoredToolOutput {
	if outputs == nil {
		return nil
	}
	out := make([]openai.StoredToolOutput, len(outputs))
	for i, output := range outputs {
		out[i] = openai.StoredToolOutput{Type: output.Type, CallID: output.CallID, Name: output.Name, Output: output.Output, Status: output.Status, Execution: output.Execution, Tools: cloneRaw(output.Tools)}
	}
	return out
}

func cloneRaw(value json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}

func cloneBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	out := *value
	return &out
}
