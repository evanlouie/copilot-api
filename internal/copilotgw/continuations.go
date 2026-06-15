package copilotgw

import (
	"context"
	"errors"
	"maps"
	"sort"
	"strings"
	"time"

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
		return nil, openai.InvalidRequest(ctx.Err().Error(), "messages")
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
			_ = g.store.SaveSessionMetadata(runner.session.SessionID, sessionstore.SessionMetadata{ID: runner.session.SessionID, Kind: "chat", OpenAIID: result.ID, SDKSessionID: runner.session.SessionID, Model: runner.model, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), RetainedPath: runner.retained, FinishReason: result.FinishReason, PendingBatchID: result.PendingBatchID})
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
	g.pendingMu.Lock()
	g.pendingRunners[batchID] = runner
	g.pendingMu.Unlock()
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
	g.pendingMu.Lock()
	defer g.pendingMu.Unlock()
	return g.pendingRunners[batchID]
}
func (g *RealGateway) forgetRunner(batchID string) {
	g.pendingMu.Lock()
	defer g.pendingMu.Unlock()
	delete(g.pendingRunners, batchID)
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

func (g *RealGateway) continueToolResponse(ctx context.Context, req ResponseRequest) (*ResponseResult, error) {
	batch, activeOutputs, err := g.responseContinuationBatch(req.FunctionOutputs)
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
	runner := g.runnerForBatch(batch.ID)
	outputs, err := functionOutputsWithContinuationInput(activeOutputs, req.Input)
	if err != nil {
		return nil, err
	}
	if err := batch.CompleteWithSetup(outputs, func() {
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
		if err := g.store.SaveResponse(record); err != nil {
			return nil, openai.Internal(err.Error())
		}
		return &ResponseResult{Response: resp}, nil
	case <-ctx.Done():
		return nil, openai.InvalidRequest(ctx.Err().Error(), "input")
	}
}

func (g *RealGateway) responseContinuationBatch(outputs map[string]string) (*toolproxy.Batch, map[string]string, error) {
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
	active := make(map[string]string, len(matched))
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
			fallback.FunctionOutputs = nil
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
	outputs, err := functionOutputsWithContinuationInput(req.FunctionOutputs, req.Input)
	if err != nil {
		return ResponseRequest{}, err
	}
	fallback := req
	fallback.FunctionOutputs = nil
	fallback.Input = openai.PromptContent{Text: responseFunctionOutputsPrompt(outputs)}
	if !fallback.StoreSet {
		fallback.Store = previousRecord.Stored
	}
	return fallback, nil
}

func responseFunctionOutputsPrompt(outputs map[string]string) string {
	ids := make([]string, 0, len(outputs))
	for id := range outputs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var b strings.Builder
	b.WriteString("Function call outputs received for the previous response:")
	for _, id := range ids {
		b.WriteString("\n\n")
		b.WriteString(id)
		b.WriteString(":\n")
		b.WriteString(outputs[id])
	}
	return b.String()
}
