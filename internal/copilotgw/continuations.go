package copilotgw

import (
	"context"
	"errors"
	"maps"
	"sort"
	"strings"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
)

func (g *RealGateway) ContinueChatToolCalls(ctx context.Context, req ChatContinuationRequest) (*TurnResult, error) {
	if err := g.ValidateModel(ctx, req.Model); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(req.Outputs))
	for id := range req.Outputs {
		ids = append(ids, id)
	}
	batch, err := g.broker.FindByCallIDs(ids)
	if err != nil {
		return g.continueChatToolCallsFromTranscript(ctx, req)
	}
	if batch.Kind != "chat" {
		return nil, openai.InvalidRequest("tool_call_id does not belong to a Chat Completions pending batch", "messages")
	}
	if err := validateContinuationModel(req.Model, batch, "model"); err != nil {
		return nil, err
	}
	runner := g.runnerForBatch(batch.ID)
	if err := batch.CompleteWithSetup(req.Outputs, func() {
		if runner != nil {
			runner.attachToRequestContext()
			runner.watchContext(ctx)
		}
	}); err != nil {
		return nil, openai.InvalidRequest(err.Error(), "messages")
	}
	g.broker.Remove(batch)
	g.forgetRunner(batch.ID)
	select {
	case final := <-batch.Done:
		if final.Err != nil {
			return nil, final.Err
		}
		turn, ok := final.Value.(*TurnResult)
		if !ok {
			return nil, openai.Internal("unexpected continuation result")
		}
		if turn.PendingBatchID != "" && runner != nil {
			g.rememberRunner(turn.PendingBatchID, runner)
		}
		return turn, nil
	case <-ctx.Done():
		return nil, requestContextError(ctx)
	}
}

func (g *RealGateway) StreamContinueChatToolCalls(ctx context.Context, req ChatContinuationRequest) (<-chan StreamEvent, error) {
	if err := g.ValidateModel(ctx, req.Model); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(req.Outputs))
	for id := range req.Outputs {
		ids = append(ids, id)
	}
	batch, err := g.broker.FindByCallIDs(ids)
	if err != nil {
		return g.streamContinueChatToolCallsFromTranscript(ctx, req)
	}
	if batch.Kind != "chat" {
		return nil, openai.InvalidRequest("tool_call_id does not belong to a Chat Completions pending batch", "messages")
	}
	if err := validateContinuationModel(req.Model, batch, "model"); err != nil {
		return nil, err
	}
	runner := g.runnerForBatch(batch.ID)
	if runner == nil {
		batch.Cancel(openai.InvalidRequest("pending tool_call_id is not attached to a live streamable turn", "messages"))
		g.broker.Remove(batch)
		g.forgetRunner(batch.ID)
		return g.streamContinueChatToolCallsFromTranscript(ctx, req)
	}
	ch := make(chan StreamEvent, 32)
	if err := batch.CompleteWithSetup(req.Outputs, func() {
		runner.attachToRequestContext()
		runner.watchContext(ctx)
		runner.enableChatStream(ch, ctx.Done())
		runner.setOnResult(func(result *TurnResult) error {
			if result.PendingBatchID != "" {
				g.rememberRunner(result.PendingBatchID, runner)
			}
			g.saveChatSessionMetadata(runner.session.SessionID, runner.retained, runner.model, result)
			return nil
		})
	}); err != nil {
		close(ch)
		return nil, openai.InvalidRequest(err.Error(), "messages")
	}
	g.broker.Remove(batch)
	g.forgetRunner(batch.ID)
	go runner.discardInitial()
	return ch, nil
}

func (g *RealGateway) continueChatToolCallsFromTranscript(ctx context.Context, req ChatContinuationRequest) (*TurnResult, error) {
	chatReq, err := chatRequestFromContinuation(req)
	if err != nil {
		return nil, err
	}
	return g.Chat(ctx, chatReq)
}

func (g *RealGateway) streamContinueChatToolCallsFromTranscript(ctx context.Context, req ChatContinuationRequest) (<-chan StreamEvent, error) {
	chatReq, err := chatRequestFromContinuation(req)
	if err != nil {
		return nil, err
	}
	return g.StreamChat(ctx, chatReq)
}

func chatRequestFromContinuation(req ChatContinuationRequest) (ChatRequest, error) {
	if len(req.Messages) == 0 {
		return ChatRequest{}, openai.InvalidRequest("unknown or expired tool_call_id", "messages")
	}
	// The transcript already ends with the tool result messages, which hydration
	// replays as tool-execution events in the synthetic session. The synthetic
	// turn therefore only needs to nudge the model to continue rather than
	// restating the same outputs, which would duplicate them in the prompt.
	history := append([]openai.ChatMessage(nil), req.Messages...)
	return ChatRequest{
		Model:                  req.Model,
		Instructions:           req.Instructions,
		History:                history,
		FinalUser:              openai.ChatMessage{Role: "user", Content: openai.NewTextContent("Continue.")},
		Tools:                  req.Tools,
		ToolChoiceNone:         req.ToolChoiceNone,
		ReasoningEffort:        req.ReasoningEffort,
		DefaultReasoningEffort: req.DefaultReasoningEffort,
		IncludeUsageChunk:      req.IncludeUsageChunk,
	}, nil
}

func (g *RealGateway) rememberRunner(batchID string, runner *turnRunner) {
	if batchID == "" || runner == nil {
		return
	}
	g.pending.put(batchID, runner)
	if batch := runner.currentBatch(); batch != nil && batch.ID == batchID {
		batch.OnExpire(func(expired *toolproxy.Batch) { g.forgetRunner(expired.ID) })
	}
}
func validateContinuationModel(model string, batch *toolproxy.Batch, param string) error {
	if batch.Model != "" && model != batch.Model {
		return openai.InvalidRequest("model does not match pending tool-call batch", param)
	}
	return nil
}
func (g *RealGateway) runnerForBatch(batchID string) *turnRunner {
	return g.pending.get(batchID)
}
func (g *RealGateway) forgetRunner(batchID string) {
	g.pending.remove(batchID)
}
func functionOutputsWithContinuationInput(outputs map[string]string, input openai.PromptContent) (map[string]string, error) {
	if strings.TrimSpace(input.Text) == "" && len(input.Images) == 0 {
		return outputs, nil
	}
	if len(input.Images) > 0 {
		return nil, openai.InvalidRequest("image input cannot be combined with function_call_output continuation input", "input")
	}
	ids := make([]string, 0, len(outputs))
	for id := range outputs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return outputs, nil
	}
	out := make(map[string]string, len(outputs))
	maps.Copy(out, outputs)
	// The Copilot SDK resumes a parked tool turn by returning strings from the
	// pending tool handlers; it has no separate channel for a same-turn user
	// message after function_call_output items. Preserve the client's follow-up
	// input by appending it to one deterministic tool result rather than dropping
	// it. Keep this adapter isolated so it can be replaced if the SDK gains native
	// mixed-continuation support.
	out[ids[0]] += "\n\nAdditional user input after tool output:\n" + input.Text
	return out, nil
}

func toolOutputsWithContinuationInput(outputs map[string]openai.ResponseToolOutput, input openai.PromptContent) (map[string]openai.ResponseToolOutput, error) {
	stringsOnly := make(map[string]string, len(outputs))
	for id, output := range outputs {
		stringsOnly[id] = output.Output
	}
	updatedStrings, err := functionOutputsWithContinuationInput(stringsOnly, input)
	if err != nil {
		return nil, err
	}
	if len(updatedStrings) == 0 {
		return outputs, nil
	}
	out := make(map[string]openai.ResponseToolOutput, len(outputs))
	maps.Copy(out, outputs)
	for id, text := range updatedStrings {
		entry := out[id]
		entry.Output = text
		out[id] = entry
	}
	return out, nil
}

func (g *RealGateway) continueToolResponse(ctx context.Context, req ResponseRequest) (*ResponseResult, error) {
	batch, activeOutputs, err := g.responseContinuationBatch(req.ToolOutputs)
	if err != nil {
		if errors.Is(err, toolproxy.ErrNotFound) {
			return g.continueToolResponseFromRecord(ctx, req)
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
		return g.continueDynamicToolSearchResponse(ctx, req, batch, activeOutputs, previousResponseID, previousRecord)
	}
	catalogDTO, err := responseCatalogDTOForRequest(req, &previousRecord)
	if err != nil {
		return nil, err
	}
	runner := g.runnerForBatch(batch.ID)
	releaseSession := g.store.PinSession(previousRecord.SDKSessionID)
	releaseResponse := g.store.PinResponse(req.ResponseID)
	defer releaseSession()
	defer releaseResponse()
	outputs, err := toolOutputsWithContinuationInput(activeOutputs, req.Input)
	if err != nil {
		return nil, err
	}
	if err := batch.CompleteToolOutputsWithSetup(outputs, func() {
		if runner != nil {
			runner.setCurrentResponseID(req.ResponseID)
			runner.attachToRequestContext()
			runner.watchContext(ctx)
		}
	}); err != nil {
		return nil, openai.InvalidRequest(err.Error(), "input")
	}
	g.broker.Remove(batch)
	g.forgetRunner(batch.ID)
	select {
	case final := <-batch.Done:
		if final.Err != nil {
			return nil, final.Err
		}
		turn, ok := final.Value.(*TurnResult)
		if !ok {
			return nil, openai.Internal("unexpected continuation result")
		}
		if turn.PendingBatchID != "" && runner != nil {
			g.rememberRunner(turn.PendingBatchID, runner)
		}
		previous := previousResponseID
		storeVisible := req.Store
		if !req.StoreSet {
			storeVisible = previousRecord.Stored
		}
		resp := responseFromTurn(req.ResponseID, req.Model, req.Instructions, &previous, storeVisible, turn, req.SuppressReasoning)
		record := recordFromResponse(resp, turn.SDKSessionID, turn.RetainedPath)
		record.PendingBatchID = turn.PendingBatchID
		record.ToolOutputs = storeToolOutputs(openai.StoredToolOutputsFromMap(outputs))
		record.InstalledToolCatalog = storeToolCatalog(catalogDTO)
		if err := g.store.SaveResponse(record); err != nil {
			return nil, openai.Internal("failed to persist response")
		}
		return &ResponseResult{Response: resp}, nil
	case <-ctx.Done():
		return nil, requestContextError(ctx)
	}
}

func (g *RealGateway) continueDynamicToolSearchResponse(ctx context.Context, req ResponseRequest, batch *toolproxy.Batch, activeOutputs map[string]openai.ResponseToolOutput, previousResponseID string, previousRecord sessionstore.ResponseRecord) (*ResponseResult, error) {
	fallback, err := g.prepareDynamicToolSearchFallback(req, batch, activeOutputs, previousResponseID, previousRecord)
	if err != nil {
		return nil, err
	}
	return g.CreateResponse(ctx, fallback)
}

func (g *RealGateway) streamDynamicToolSearchResponse(ctx context.Context, req ResponseRequest, batch *toolproxy.Batch, activeOutputs map[string]openai.ResponseToolOutput, previousResponseID string, previousRecord sessionstore.ResponseRecord) (<-chan ResponseStreamEvent, error) {
	fallback, err := g.prepareDynamicToolSearchFallback(req, batch, activeOutputs, previousResponseID, previousRecord)
	if err != nil {
		return nil, err
	}
	return g.StreamResponse(ctx, fallback)
}

func (g *RealGateway) prepareDynamicToolSearchFallback(req ResponseRequest, batch *toolproxy.Batch, activeOutputs map[string]openai.ResponseToolOutput, previousResponseID string, previousRecord sessionstore.ResponseRecord) (ResponseRequest, error) {
	outputs, err := toolOutputsWithContinuationInput(activeOutputs, req.Input)
	if err != nil {
		return ResponseRequest{}, err
	}
	merge, err := mergeLoadedToolSearchOutputs(req, previousRecord, outputs)
	if err != nil {
		return ResponseRequest{}, err
	}
	if runner := g.runnerForBatch(batch.ID); runner != nil {
		runner.abort()
	} else {
		batch.Cancel(context.Canceled)
	}
	g.broker.Remove(batch)
	g.forgetRunner(batch.ID)
	storeVisible := req.Store
	if !req.StoreSet {
		storeVisible = previousRecord.Stored
	}
	fallback := req
	fallback.ToolOutputs = nil
	fallback.Input = openai.PromptContent{Text: responseToolOutputsPrompt(outputs, wireOutputItems(previousRecord.Output))}
	fallback.PreviousResponseID = previousResponseID
	fallback.Tools = merge.Catalog.Flatten()
	fallback.ToolsSet = true
	fallback.ForceSynthetic = true
	fallback.Store = storeVisible
	fallback.StoreSet = true
	fallback.ContinuationToolOutputs = outputs
	fallback.LoadedToolEvents = append(append([]openai.StoredLoadedToolEvent{}, req.LoadedToolEvents...), merge.Events...)
	return fallback, nil
}

func (g *RealGateway) responseContinuationBatch(outputs map[string]openai.ResponseToolOutput) (*toolproxy.Batch, map[string]openai.ResponseToolOutput, error) {
	ids := make([]string, 0, len(outputs))
	for id := range outputs {
		ids = append(ids, id)
	}
	batch, err := g.broker.FindByCallIDs(ids)
	if err == nil {
		return batch, outputs, nil
	}
	// Codex's HTTP transport can resend the full stateless Responses input,
	// including old function_call_output items whose live batches were already
	// consumed. If exactly one current live batch is present, continue with just
	// that batch's outputs and ignore stale history items.
	batch, matched, subsetErr := g.broker.FindByAnyCallIDs(ids)
	if subsetErr != nil {
		if errors.Is(subsetErr, toolproxy.ErrNotFound) {
			return nil, nil, err
		}
		return nil, nil, openai.InvalidRequest(subsetErr.Error(), "input")
	}
	active := make(map[string]openai.ResponseToolOutput, len(matched))
	for _, id := range matched {
		active[id] = outputs[id]
	}
	return batch, active, nil
}

func (g *RealGateway) continueToolResponseFromRecord(ctx context.Context, req ResponseRequest) (*ResponseResult, error) {
	fallback, err := g.responseFallbackRequestFromFunctionOutputs(req)
	if err != nil {
		return nil, err
	}
	return g.CreateResponse(ctx, fallback)
}

func (g *RealGateway) streamToolResponseFromRecord(ctx context.Context, req ResponseRequest) (<-chan ResponseStreamEvent, error) {
	fallback, err := g.responseFallbackRequestFromFunctionOutputs(req)
	if err != nil {
		return nil, err
	}
	return g.StreamResponse(ctx, fallback)
}

func (g *RealGateway) responseFallbackRequestFromFunctionOutputs(req ResponseRequest) (ResponseRequest, error) {
	if req.PreviousResponseID == "" {
		if req.FunctionOutputFallbackAvailable {
			fallback := req
			fallback.ToolOutputs = nil
			fallback.FunctionOutputFallbackInput = openai.PromptContent{}
			fallback.FunctionOutputFallbackInstructions = ""
			fallback.FunctionOutputFallbackAvailable = false
			fallback.Input = req.FunctionOutputFallbackInput
			fallback.Instructions = req.FunctionOutputFallbackInstructions
			return fallback, nil
		}
		return ResponseRequest{}, openai.InvalidRequest("previous_response_id is required when function_call_output is no longer attached to a live pending batch", "previous_response_id")
	}
	previousRecord, err := g.store.LoadResponseForContinuation(req.PreviousResponseID)
	if err != nil {
		return ResponseRequest{}, openai.PreviousResponseNotFound(req.PreviousResponseID)
	}
	activeOutputs, err := activeResponseToolOutputsFromRecord(previousRecord, req.ToolOutputs)
	if err != nil {
		return ResponseRequest{}, err
	}
	outputs, err := toolOutputsWithContinuationInput(activeOutputs, req.Input)
	if err != nil {
		return ResponseRequest{}, err
	}
	merge, err := mergeLoadedToolSearchOutputs(req, previousRecord, outputs)
	if err != nil {
		return ResponseRequest{}, err
	}
	fallback := req
	fallback.ToolOutputs = nil
	fallback.Input = openai.PromptContent{Text: responseToolOutputsPrompt(outputs, wireOutputItems(previousRecord.Output))}
	fallback.Tools = merge.Catalog.Flatten()
	fallback.ToolsSet = true
	fallback.ForceSynthetic = true
	fallback.ContinuationToolOutputs = outputs
	fallback.LoadedToolEvents = append(append([]openai.StoredLoadedToolEvent{}, req.LoadedToolEvents...), merge.Events...)
	if !fallback.StoreSet {
		fallback.Store = previousRecord.Stored
	}
	return fallback, nil
}

func responseToolOutputsPrompt(outputs map[string]openai.ResponseToolOutput, previousItems []openai.ResponseOutputItem) string {
	ids := make([]string, 0, len(outputs))
	for id := range outputs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	previousByCallID := map[string]openai.ResponseOutputItem{}
	for _, item := range previousItems {
		if item.CallID != "" {
			previousByCallID[item.CallID] = item
		}
	}
	var b strings.Builder
	b.WriteString("Tool outputs received for the previous response:")
	for _, id := range ids {
		output := outputs[id]
		previous := previousByCallID[id]
		b.WriteString("\n\n")
		b.WriteString(responseToolOutputPromptHeader(id, output, previous))
		if previous.Type != "" {
			b.WriteString("\nAssistant call: ")
			b.WriteString(responseOutputItemPromptSummary(previous))
		}
		if len(output.Tools) > 0 {
			b.WriteString("\nReturned tools: ")
			b.WriteString(string(output.Tools))
		}
		b.WriteString("\nOutput:\n")
		b.WriteString(output.Output)
	}
	return b.String()
}

func responseToolOutputPromptHeader(id string, output openai.ResponseToolOutput, previous openai.ResponseOutputItem) string {
	kind := output.Kind
	if kind == "" {
		switch previous.Type {
		case "custom_tool_call":
			kind = openai.ToolKindCustom
		case "tool_search_call":
			kind = openai.ToolKindToolSearch
		default:
			kind = openai.ToolKindFunction
		}
	}
	name := output.Name
	if name == "" {
		name = previous.Name
	}
	if previous.Namespace != "" && name != "" {
		name = previous.Namespace + "." + name
	}
	suffix := ""
	if name != "" {
		suffix = " for " + name
	}
	switch kind {
	case openai.ToolKindCustom:
		return "Custom tool output " + id + suffix + ":"
	case openai.ToolKindToolSearch:
		extra := ""
		if output.Execution != "" || output.Status != "" {
			parts := []string{}
			if output.Execution != "" {
				parts = append(parts, "execution="+output.Execution)
			}
			if output.Status != "" {
				parts = append(parts, "status="+output.Status)
			}
			extra = " (" + strings.Join(parts, ", ") + ")"
		}
		return "Tool search output " + id + extra + ":"
	default:
		return "Function output " + id + suffix + ":"
	}
}

func responseOutputItemPromptSummary(item openai.ResponseOutputItem) string {
	switch item.Type {
	case "custom_tool_call":
		return "custom_tool_call " + item.Name + " input=" + item.Input
	case "tool_search_call":
		args := item.Arguments
		if args == "" && len(item.ArgumentsJSON) > 0 {
			args = string(item.ArgumentsJSON)
		}
		return "tool_search_call arguments=" + args
	case "function_call":
		name := item.Name
		if item.Namespace != "" {
			name = item.Namespace + "." + name
		}
		return "function_call " + name + " arguments=" + item.Arguments
	default:
		return item.Type
	}
}
