package copilotgw

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
)

func (g *RealGateway) ContinueChatToolCalls(ctx context.Context, model string, outputs map[string]string) (*TurnResult, error) {
	if err := g.ValidateModel(ctx, model); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(outputs))
	for id := range outputs {
		ids = append(ids, id)
	}
	batch, err := g.broker.FindByCallIDs(ids)
	if err != nil {
		return nil, openai.InvalidRequest("unknown or expired tool_call_id", "messages")
	}
	if batch.Kind != "chat" {
		return nil, openai.InvalidRequest("tool_call_id does not belong to a Chat Completions pending batch", "messages")
	}
	if err := validateContinuationModel(model, batch, "model"); err != nil {
		return nil, err
	}
	runner := g.runnerForBatch(batch.ID)
	if err := batch.Complete(outputs); err != nil {
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
func (g *RealGateway) StreamContinueChatToolCalls(ctx context.Context, model string, outputs map[string]string) (<-chan StreamEvent, error) {
	if err := g.ValidateModel(ctx, model); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(outputs))
	for id := range outputs {
		ids = append(ids, id)
	}
	batch, err := g.broker.FindByCallIDs(ids)
	if err != nil {
		return nil, openai.InvalidRequest("unknown or expired tool_call_id", "messages")
	}
	if batch.Kind != "chat" {
		return nil, openai.InvalidRequest("tool_call_id does not belong to a Chat Completions pending batch", "messages")
	}
	if err := validateContinuationModel(model, batch, "model"); err != nil {
		return nil, err
	}
	runner := g.runnerForBatch(batch.ID)
	if runner == nil {
		return nil, openai.InvalidRequest("pending tool_call_id is not attached to a live streamable turn", "messages")
	}
	ch := make(chan StreamEvent, 32)
	if err := batch.CompleteWithSetup(outputs, func() {
		runner.enableChatStream(ch)
		runner.setOnResult(func(result *TurnResult) {
			if result.PendingBatchID != "" {
				g.rememberRunner(result.PendingBatchID, runner)
			}
			_ = g.store.SaveSessionMetadata(runner.session.SessionID, sessionstore.SessionMetadata{ID: runner.session.SessionID, Kind: "chat", OpenAIID: result.ID, SDKSessionID: runner.session.SessionID, Model: runner.model, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), RetainedPath: runner.retained, FinishReason: result.FinishReason, PendingBatchID: result.PendingBatchID})
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
	for id, value := range outputs {
		out[id] = value
	}
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
	ids := make([]string, 0, len(req.FunctionOutputs))
	for id := range req.FunctionOutputs {
		ids = append(ids, id)
	}
	batch, err := g.broker.FindByCallIDs(ids)
	if err != nil {
		return nil, openai.InvalidRequest("unknown or expired function_call_output call_id", "input")
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
	outputs, err := functionOutputsWithContinuationInput(req.FunctionOutputs, req.Input)
	if err != nil {
		return nil, err
	}
	if err := batch.Complete(outputs); err != nil {
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
		resp := responseFromTurn(req.ResponseID, req.Model, req.Instructions, &previous, storeVisible, turn)
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
