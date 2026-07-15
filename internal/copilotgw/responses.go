package copilotgw

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	copilot "github.com/github/copilot-sdk/go"
)

func (g *RealGateway) CreateResponse(ctx context.Context, req ResponseRequest) (*ResponseResult, error) {
	if err := g.ValidateModel(ctx, req.Model); err != nil {
		return nil, err
	}
	if req.ResponseID == "" {
		req.ResponseID = openai.NewID("resp_")
	}
	if len(req.ToolOutputs) > 0 {
		return g.continueToolResponse(ctx, req)
	}
	incrementalInput := req.Input.Text
	prepared, err := g.prepareResponseTurn(ctx, &req, false)
	if err != nil {
		return nil, err
	}
	runner := g.newTurnRunner(ctx, req.ResponseID, req.Model, prepared.session, prepared.rt, prepared.events, prepared.retained, "response", req.ResponseID)
	runner.watchContext(ctx)
	if _, err := prepared.session.Send(ctx, copilot.MessageOptions{Prompt: prepared.prompt.Text, Attachments: prepared.prompt.Attachments}); err != nil {
		runner.failSend(prepared.events, err)
		_, _ = runner.waitInitial(ctx)
		return nil, openai.Upstream(err.Error())
	}
	turn, err := runner.waitInitial(ctx)
	if err != nil {
		return nil, err
	}
	if turn.PendingBatchID != "" {
		g.rememberRunner(turn.PendingBatchID, runner)
	}
	resp := responseFromTurn(req.ResponseID, req.Model, req.Instructions, prepared.previous, req.Store, turn, req.SuppressReasoning)
	record := recordFromResponse(resp, prepared.sessionID, prepared.retained)
	record.InputText = incrementalInput
	record.PendingBatchID = turn.PendingBatchID
	record.InstalledToolCatalog = prepared.catalog.StoredDTO()
	record.ToolOutputs = openai.StoredToolOutputsFromMap(req.ContinuationToolOutputs)
	record.LoadedToolEvents = append([]openai.StoredLoadedToolEvent{}, req.LoadedToolEvents...)
	if err := g.store.SaveResponse(record); err != nil {
		return nil, openai.Internal("failed to persist response")
	}
	return &ResponseResult{Response: resp}, nil
}
func (g *RealGateway) StreamResponse(ctx context.Context, req ResponseRequest) (<-chan ResponseStreamEvent, error) {
	if err := g.ValidateModel(ctx, req.Model); err != nil {
		return nil, err
	}
	if req.ResponseID == "" {
		req.ResponseID = openai.NewID("resp_")
	}
	if len(req.ToolOutputs) > 0 {
		batch, activeOutputs, err := g.responseContinuationBatch(req.ToolOutputs)
		if err != nil {
			if errors.Is(err, toolproxy.ErrNotFound) {
				return g.streamToolResponseFromRecord(ctx, req)
			}
			return nil, err
		}
		previousResponseID := req.PreviousResponseID
		if previousResponseID == "" {
			previousResponseID = batch.ResponseID
		}
		if batch.Kind != "response" {
			return nil, openai.InvalidRequest("function_call_output call_id does not belong to a Responses pending batch", "input")
		}
		if err := validateContinuationModel(req.Model, batch, "model"); err != nil {
			return nil, err
		}
		if batch.ResponseID != "" && previousResponseID != batch.ResponseID {
			return nil, openai.InvalidRequest("function_call_output call_id does not belong to previous_response_id", "input")
		}
		previousRecord, err := g.store.LoadResponseForContinuation(previousResponseID)
		if err != nil {
			return nil, openai.PreviousResponseNotFound(previousResponseID)
		}
		installBoundary, err := validateResponseToolOutputsForBatch(batch, activeOutputs)
		if err != nil {
			return nil, err
		}
		if installBoundary {
			return g.streamDynamicToolSearchResponse(ctx, req, batch, activeOutputs, previousResponseID, previousRecord)
		}
		catalogDTO, err := responseCatalogDTOForRequest(req, &previousRecord)
		if err != nil {
			return nil, err
		}
		runner := g.runnerForBatch(batch.ID)
		if runner == nil {
			batch.Cancel(openai.InvalidRequest("pending function_call_output is not attached to a live streamable turn", "input"))
			g.broker.Remove(batch)
			g.forgetRunner(batch.ID)
			return g.streamToolResponseFromRecord(ctx, req)
		}
		storeVisible := req.Store
		if !req.StoreSet {
			storeVisible = previousRecord.Stored
		}
		outputs, err := toolOutputsWithContinuationInput(activeOutputs, req.Input)
		if err != nil {
			return nil, err
		}
		previous := previousResponseID
		ch := make(chan ResponseStreamEvent, 32)
		if err := batch.CompleteToolOutputsWithSetup(outputs, func() {
			runner.attachToRequestContext()
			runner.watchContext(ctx)
			runner.enableResponseStream(ch, req.ResponseID, req.Model, req.Instructions, &previous, storeVisible, req.SuppressReasoning, ctx.Done())
			runner.setOnResult(func(turn *TurnResult) error {
				if turn.PendingBatchID != "" {
					g.rememberRunner(turn.PendingBatchID, runner)
				}
				resp := responseFromTurn(req.ResponseID, req.Model, req.Instructions, &previous, storeVisible, turn, req.SuppressReasoning)
				record := recordFromResponse(resp, turn.SDKSessionID, turn.RetainedPath)
				record.PendingBatchID = turn.PendingBatchID
				record.ToolOutputs = openai.StoredToolOutputsFromMap(outputs)
				record.InstalledToolCatalog = catalogDTO
				if err := g.store.SaveResponse(record); err != nil {
					return openai.Internal("failed to persist response")
				}
				return nil
			})
		}); err != nil {
			close(ch)
			return nil, openai.InvalidRequest(err.Error(), "input")
		}
		g.broker.Remove(batch)
		g.forgetRunner(batch.ID)
		go runner.discardInitial()
		return ch, nil
	}
	incrementalInput := req.Input.Text
	prepared, err := g.prepareResponseTurn(ctx, &req, true)
	if err != nil {
		return nil, err
	}
	ch := make(chan ResponseStreamEvent, 32)
	runner := g.newTurnRunner(ctx, req.ResponseID, req.Model, prepared.session, prepared.rt, prepared.events, prepared.retained, "response", req.ResponseID)
	runner.watchContext(ctx)
	runner.enableResponseStream(ch, req.ResponseID, req.Model, req.Instructions, prepared.previous, req.Store, req.SuppressReasoning, ctx.Done())
	runner.setOnResult(func(turn *TurnResult) error {
		if turn.PendingBatchID != "" {
			g.rememberRunner(turn.PendingBatchID, runner)
		}
		resp := responseFromTurn(req.ResponseID, req.Model, req.Instructions, prepared.previous, req.Store, turn, req.SuppressReasoning)
		record := recordFromResponse(resp, prepared.sessionID, prepared.retained)
		record.InputText = incrementalInput
		record.PendingBatchID = turn.PendingBatchID
		record.InstalledToolCatalog = prepared.catalog.StoredDTO()
		record.ToolOutputs = openai.StoredToolOutputsFromMap(req.ContinuationToolOutputs)
		record.LoadedToolEvents = append([]openai.StoredLoadedToolEvent{}, req.LoadedToolEvents...)
		if err := g.store.SaveResponse(record); err != nil {
			return openai.Internal("failed to persist response")
		}
		return nil
	})
	go runner.discardInitial()
	go func() {
		runner.debug(g, "copilot send started", "prompt_bytes", len(prepared.prompt.Text), "attachment_count", len(prepared.prompt.Attachments))
		if _, err := prepared.session.Send(ctx, copilot.MessageOptions{Prompt: prepared.prompt.Text, Attachments: prepared.prompt.Attachments}); err != nil {
			runner.debug(g, "copilot send failed", "error", err.Error())
			runner.failSend(prepared.events, err)
			return
		}
		runner.debug(g, "copilot send returned")
	}()
	return ch, nil
}
func (g *RealGateway) responseContinuationPrompt(previous sessionstore.ResponseRecord, current resolvedPrompt) resolvedPrompt {
	records := []sessionstore.ResponseRecord{previous}
	seen := map[string]struct{}{previous.ID: {}}
	for id := previous.PreviousResponseID; id != "" && len(records) < 20; {
		if _, ok := seen[id]; ok {
			break
		}
		seen[id] = struct{}{}
		record, err := g.store.LoadResponseForContinuation(id)
		if err != nil || record.Deleted {
			break
		}
		records = append(records, record)
		id = record.PreviousResponseID
	}
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}

	var b strings.Builder
	b.WriteString("Conversation so far from previous_response_id context:\n\n")
	for _, record := range records {
		appendResponseRecordTranscript(&b, record)
	}
	if text := strings.TrimSpace(current.Text); text != "" {
		b.WriteString("Current user request:\n")
		b.WriteString(text)
	} else {
		b.WriteString("Current user request:")
	}
	current.Text = b.String()
	return current
}

func appendResponseRecordTranscript(b *strings.Builder, record sessionstore.ResponseRecord) {
	if text := strings.TrimSpace(record.InputText); text != "" {
		b.WriteString("User:\n")
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	if text := strings.TrimSpace(record.OutputText); text != "" {
		b.WriteString("Assistant:\n")
		b.WriteString(text)
		b.WriteString("\n\n")
	}
	for _, item := range record.Output {
		switch item.Type {
		case "function_call", "custom_tool_call", "tool_search_call":
			b.WriteString("Assistant call: ")
			b.WriteString(responseOutputItemPromptSummary(item))
			if item.CallID != "" {
				b.WriteString(" call_id=")
				b.WriteString(item.CallID)
			}
			b.WriteString("\n\n")
		}
	}
	for _, output := range record.ToolOutputs {
		b.WriteString(storedToolOutputPrompt(output))
		b.WriteString("\n\n")
	}
	for _, event := range record.LoadedToolEvents {
		if len(event.LoadedTools) == 0 {
			continue
		}
		b.WriteString("Loaded tools from tool search ")
		b.WriteString(event.SourceCallID)
		b.WriteString(": ")
		b.WriteString(storedToolNames(event.LoadedTools))
		b.WriteString("\n\n")
	}
}

func storedToolOutputPrompt(output openai.StoredToolOutput) string {
	var b strings.Builder
	switch output.Type {
	case "custom_tool_call_output":
		b.WriteString("Custom tool output ")
	case "tool_search_output":
		b.WriteString("Tool search output ")
	default:
		b.WriteString("Function output ")
	}
	b.WriteString(output.CallID)
	if output.Name != "" {
		b.WriteString(" for ")
		b.WriteString(output.Name)
	}
	if output.Execution != "" || output.Status != "" {
		parts := []string{}
		if output.Execution != "" {
			parts = append(parts, "execution="+output.Execution)
		}
		if output.Status != "" {
			parts = append(parts, "status="+output.Status)
		}
		b.WriteString(" (")
		b.WriteString(strings.Join(parts, ", "))
		b.WriteString(")")
	}
	b.WriteString(":\n")
	if len(output.Tools) > 0 {
		b.WriteString("Returned tools: ")
		b.WriteString(string(output.Tools))
		b.WriteString("\n")
	}
	b.WriteString(output.Output)
	return b.String()
}

func storedToolNames(tools []openai.StoredToolSpec) string {
	parts := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool.Type == openai.ToolKindNamespace {
			for _, child := range tool.Tools {
				parts = append(parts, tool.Name+"."+child.Name)
			}
			if len(tool.Tools) == 0 {
				parts = append(parts, tool.Name)
			}
			continue
		}
		name := tool.Name
		if tool.Namespace != "" {
			name = tool.Namespace + "." + tool.Name
		}
		parts = append(parts, name)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func (g *RealGateway) GetResponse(ctx context.Context, id string) (*openai.Response, error) {
	record, err := g.store.LoadResponse(id)
	if err != nil {
		if errors.Is(err, sessionstore.ErrNotFound) {
			return nil, openai.NotFound("response not found", "not_found")
		}
		return nil, openai.Internal("failed to load response")
	}
	resp := &openai.Response{ID: record.ID, Object: openai.ObjectResponse, CreatedAt: record.CreatedAt.Unix(), Status: record.Status, Model: record.Model, Instructions: record.Instructions, Output: record.Output, OutputText: record.OutputText, Store: record.Stored, Usage: record.Usage, Error: nil, IncompleteDetails: nil, ParallelToolCalls: true}
	if record.PreviousResponseID != "" {
		resp.PreviousResponseID = &record.PreviousResponseID
	}
	return resp, nil
}
func (g *RealGateway) DeleteResponse(ctx context.Context, id string) error {
	if err := g.store.DeleteResponse(id); err != nil {
		if errors.Is(err, sessionstore.ErrNotFound) {
			return openai.NotFound("response not found", "not_found")
		}
		return openai.Internal("failed to delete response")
	}
	return nil
}
