package copilotgw

import (
	"context"
	"errors"
	"strings"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/google/uuid"
)

func (g *RealGateway) CreateResponse(ctx context.Context, req ResponseRequest) (*ResponseResult, error) {
	if err := g.ValidateModel(ctx, req.Model); err != nil {
		return nil, err
	}
	if req.ResponseID == "" {
		req.ResponseID = openai.NewID("resp_")
	}
	storeVisible := req.Store
	if !storeVisible {
		// OpenAI Responses defaults store to true. The http layer sets this explicitly.
		storeVisible = false
	}

	if len(req.FunctionOutputs) > 0 {
		return g.continueToolResponse(ctx, req)
	}
	reasoningEffort, err := g.requestReasoningEffort(ctx, req.Model, req.ReasoningEffort, req.DefaultReasoningEffort, req.ResolvedReasoningEffort, req.ReasoningEffortResolved)
	if err != nil {
		return nil, err
	}
	prompt, err := g.resolvePrompt(ctx, req.Model, req.Input, "input")
	if err != nil {
		return nil, err
	}

	rt, err := toolproxy.NewRequestTools(g.broker, req.Tools, req.ToolChoiceNone)
	if err != nil {
		return nil, openai.InvalidRequest(err.Error(), "tools")
	}
	events := make(chan copilot.SessionEvent, 256)
	var session *copilot.Session
	var sessionID string
	var previous *string
	if req.PreviousResponseID != "" {
		record, err := g.store.LoadResponseForContinuation(req.PreviousResponseID)
		if err != nil {
			return nil, openai.PreviousResponseNotFound(req.PreviousResponseID)
		}
		sessionID = record.SDKSessionID
		previous = &req.PreviousResponseID
		session, err = g.resumeSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, false, events)
		if err != nil || session == nil {
			if g.log != nil {
				g.log.Warn("falling back to synthetic Responses continuation", "previous_response_id", req.PreviousResponseID, "sdk_session_id", sessionID, "error", err)
			}
			prompt = g.responseContinuationPrompt(record, prompt)
			sessionID = "resp_sdk_" + uuid.NewString()
			session, err = g.createSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, false, events)
		}
	} else {
		sessionID = "resp_sdk_" + uuid.NewString()
		session, err = g.createSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, false, events)
	}
	if err != nil {
		return nil, openai.Upstream(err.Error())
	}
	if session == nil {
		return nil, openai.Upstream("copilot SDK returned nil session")
	}
	retained := g.fs.SessionRoot(sessionID)
	runner := g.newTurnRunner(ctx, req.ResponseID, req.Model, session, rt, events, retained, "response", req.ResponseID)
	runner.watchContext(ctx)
	if _, err := session.Send(ctx, copilot.MessageOptions{Prompt: prompt.Text, Attachments: prompt.Attachments}); err != nil {
		_ = session.Disconnect()
		return nil, openai.Upstream(err.Error())
	}
	turn, err := runner.waitInitial(ctx)
	if err != nil {
		return nil, err
	}
	if turn.PendingBatchID != "" {
		g.rememberRunner(turn.PendingBatchID, runner)
	}
	resp := responseFromTurn(req.ResponseID, req.Model, req.Instructions, previous, storeVisible, turn, req.SuppressReasoning)
	record := recordFromResponse(resp, sessionID, retained)
	record.InputText = req.Input.Text
	record.PendingBatchID = turn.PendingBatchID
	if err := g.store.SaveResponse(record); err != nil {
		return nil, openai.Internal(err.Error())
	}
	return &ResponseResult{Response: resp}, nil
}
func (g *RealGateway) StreamResponse(ctx context.Context, req ResponseRequest) (<-chan ResponseStreamEvent, error) {
	if err := g.ValidateModel(ctx, req.Model); err != nil {
		return nil, err
	}
	if len(req.FunctionOutputs) > 0 {
		ids := make([]string, 0, len(req.FunctionOutputs))
		for id := range req.FunctionOutputs {
			ids = append(ids, id)
		}
		batch, err := g.broker.FindByCallIDs(ids)
		if err != nil {
			return g.streamToolResponseFromRecord(ctx, req)
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
		outputs, err := functionOutputsWithContinuationInput(req.FunctionOutputs, req.Input)
		if err != nil {
			return nil, err
		}
		previous := previousResponseID
		ch := make(chan ResponseStreamEvent, 32)
		if err := batch.CompleteWithSetup(outputs, func() {
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
				if err := g.store.SaveResponse(record); err != nil {
					return openai.Internal(err.Error())
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
	reasoningEffort, err := g.requestReasoningEffort(ctx, req.Model, req.ReasoningEffort, req.DefaultReasoningEffort, req.ResolvedReasoningEffort, req.ReasoningEffortResolved)
	if err != nil {
		return nil, err
	}
	if req.ResponseID == "" {
		req.ResponseID = openai.NewID("resp_")
	}
	events := make(chan copilot.SessionEvent, 256)
	var session *copilot.Session
	var sessionID string
	var previous *string
	var rt *toolproxy.RequestTools
	var prompt resolvedPrompt
	retained := ""
	if warmSession, warmTools, warmEvents, warmRetained, warmPrevious, ok := req.WarmSession.use(&req); ok {
		session = warmSession
		rt = warmTools
		events = warmEvents
		retained = warmRetained
		previous = warmPrevious
		sessionID = session.SessionID
	} else {
		if req.WarmSession != nil && req.WarmSession.ResponseID() == req.PreviousResponseID {
			req.WarmSession.Disconnect()
		}
		rt, err = toolproxy.NewRequestTools(g.broker, req.Tools, req.ToolChoiceNone)
		if err != nil {
			return nil, openai.InvalidRequest(err.Error(), "tools")
		}
		prompt, err = g.resolvePrompt(ctx, req.Model, req.Input, "input")
		if err != nil {
			return nil, err
		}
		if req.PreviousResponseID != "" {
			record, err := g.store.LoadResponseForContinuation(req.PreviousResponseID)
			if err != nil {
				return nil, openai.PreviousResponseNotFound(req.PreviousResponseID)
			}
			sessionID = record.SDKSessionID
			previous = &req.PreviousResponseID
			session, err = g.resumeSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, true, events)
			if err != nil || session == nil {
				if g.log != nil {
					g.log.Warn("falling back to synthetic streaming Responses continuation", "previous_response_id", req.PreviousResponseID, "sdk_session_id", sessionID, "error", err)
				}
				prompt = g.responseContinuationPrompt(record, prompt)
				sessionID = "resp_sdk_" + uuid.NewString()
				session, err = g.createSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, true, events)
			}
		} else {
			sessionID = "resp_sdk_" + uuid.NewString()
			session, err = g.createSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, true, events)
		}
	}
	if session != nil && prompt.Text == "" && len(prompt.Attachments) == 0 {
		prompt, err = g.resolvePrompt(ctx, req.Model, req.Input, "input")
		if err != nil {
			_ = session.Disconnect()
			return nil, err
		}
	}
	if err != nil {
		return nil, openai.Upstream(err.Error())
	}
	if session == nil {
		return nil, openai.Upstream("copilot SDK returned nil session")
	}
	if retained == "" {
		retained = g.fs.SessionRoot(sessionID)
	}
	ch := make(chan ResponseStreamEvent, 32)
	runner := g.newTurnRunner(ctx, req.ResponseID, req.Model, session, rt, events, retained, "response", req.ResponseID)
	runner.watchContext(ctx)
	runner.enableResponseStream(ch, req.ResponseID, req.Model, req.Instructions, previous, req.Store, req.SuppressReasoning, ctx.Done())
	runner.setOnResult(func(turn *TurnResult) error {
		if turn.PendingBatchID != "" {
			g.rememberRunner(turn.PendingBatchID, runner)
		}
		resp := responseFromTurn(req.ResponseID, req.Model, req.Instructions, previous, req.Store, turn, req.SuppressReasoning)
		record := recordFromResponse(resp, sessionID, retained)
		record.InputText = req.Input.Text
		record.PendingBatchID = turn.PendingBatchID
		if err := g.store.SaveResponse(record); err != nil {
			return openai.Internal(err.Error())
		}
		return nil
	})
	go runner.discardInitial()
	go func() {
		runner.debug(g, "copilot send started", "prompt_bytes", len(prompt.Text), "attachment_count", len(prompt.Attachments))
		if _, err := session.Send(ctx, copilot.MessageOptions{Prompt: prompt.Text, Attachments: prompt.Attachments}); err != nil {
			runner.debug(g, "copilot send failed", "error", err.Error())
			runner.failSend(events, err)
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
func (g *RealGateway) GetResponse(ctx context.Context, id string) (*openai.Response, error) {
	record, err := g.store.LoadResponse(id)
	if err != nil {
		if errors.Is(err, sessionstore.ErrNotFound) {
			return nil, openai.NotFound("response not found", "not_found")
		}
		return nil, openai.Internal(err.Error())
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
		return openai.Internal(err.Error())
	}
	return nil
}
