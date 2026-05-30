package copilotgw

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolproxy"

	copilot "github.com/github/copilot-sdk/go"
)

type turnRunner struct {
	id       string
	model    string
	session  *copilot.Session
	rt       *toolproxy.RequestTools
	events   <-chan copilot.SessionEvent
	retained string
	kind     string

	responseID string
	created    int64
	batch      *toolproxy.Batch
	updates    chan toolproxy.TurnFinalResult

	chatStream     chan<- StreamEvent
	mu             sync.Mutex
	responseStream chan<- ResponseStreamEvent
	responseMeta   *responseStreamMeta
	onResult       func(*TurnResult)
}

type responseStreamMeta struct {
	responseID   string
	model        string
	instructions string
	previous     *string
	store        bool
}

func (g *RealGateway) newTurnRunner(id, model string, session *copilot.Session, rt *toolproxy.RequestTools, events <-chan copilot.SessionEvent, retained string, kind string, responseID string) *turnRunner {
	if id == "" {
		if kind == "response" {
			id = openai.NewID("resp_")
		} else {
			id = openai.NewID("chatcmpl_")
		}
	}
	r := &turnRunner{id: id, model: model, session: session, rt: rt, events: events, retained: retained, kind: kind, responseID: responseID, updates: make(chan toolproxy.TurnFinalResult, 16), created: openai.UnixNow()}
	go r.loop(g)
	return r
}

func (r *turnRunner) discardInitial() {
	<-r.updates
}

func (r *turnRunner) waitInitial(ctx context.Context) (*TurnResult, error) {
	select {
	case first := <-r.updates:
		if first.Err != nil {
			return nil, first.Err
		}
		res, ok := first.Value.(*TurnResult)
		if !ok {
			return nil, openai.Internal(fmt.Sprintf("unexpected turn result %T", first.Value))
		}
		return res, nil
	case <-ctx.Done():
		return nil, openai.InvalidRequest(ctx.Err().Error(), "request")
	}
}

func (r *turnRunner) enableChatStream(ch chan<- StreamEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chatStream = ch
}

func (r *turnRunner) enableResponseStream(ch chan<- ResponseStreamEvent, responseID, model, instructions string, previous *string, store bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.responseStream = ch
	if ch == nil {
		r.responseMeta = nil
		return
	}
	r.responseMeta = &responseStreamMeta{responseID: responseID, model: model, instructions: instructions, previous: previous, store: store}
}

func (r *turnRunner) loop(g *RealGateway) {
	defer r.closeStreams()
	var text string
	var reasoning string
	var usage *openai.Usage
	for event := range r.events {
		switch d := event.Data.(type) {
		case *copilot.AssistantMessageDeltaData:
			if d.DeltaContent != "" {
				r.emitDelta(d.DeltaContent)
			}
		case *copilot.AssistantReasoningData:
			reasoning = d.Content
		case *copilot.AssistantMessageData:
			if len(d.ToolRequests) > 0 {
				text = d.Content
				batch, calls := r.rt.CaptureRequests(d.ToolRequests, r.responseID, r.kind, r.updates, func() { _ = r.session.Abort(context.Background()); _ = r.session.Disconnect() })
				r.batch = batch
				res := r.result(text, reasoning, usage, "tool_calls")
				res.ToolCalls = calls
				res.PendingBatchID = batch.ID
				r.emitResult(res)
			} else {
				text = d.Content
			}
		case *copilot.AssistantUsageData:
			usage = usageFromSDK(d)
		case *copilot.SessionErrorData:
			err := openai.Upstream(d.Message)
			r.emitError(err)
			_ = r.session.Disconnect()
			return
		case *copilot.SessionIdleData:
			res := r.result(text, reasoning, usage, "stop")
			r.emitResult(res)
			_ = r.session.Disconnect()
			return
		}
	}
	r.emitError(openai.Upstream("copilot session event stream ended before idle"))
}

func (r *turnRunner) emitDelta(delta string) {
	r.mu.Lock()
	chatStream := r.chatStream
	responseStream := r.responseStream
	r.mu.Unlock()
	if chatStream != nil {
		chatStream <- StreamEvent{Kind: "delta", Delta: delta}
	}
	if responseStream != nil {
		responseStream <- ResponseStreamEvent{Kind: "delta", Delta: delta}
	}
}

func (r *turnRunner) emitResult(res *TurnResult) {
	if r.onResult != nil {
		r.onResult(res)
	}
	r.updates <- toolproxy.TurnFinalResult{Value: res}
	r.mu.Lock()
	chatStream := r.chatStream
	responseStream := r.responseStream
	meta := r.responseMeta
	if res.FinishReason == "tool_calls" {
		r.chatStream = nil
		r.responseStream = nil
		r.responseMeta = nil
	}
	r.mu.Unlock()
	if chatStream != nil {
		chatStream <- StreamEvent{Kind: "result", Result: res}
		if res.FinishReason == "tool_calls" {
			close(chatStream)
		}
	}
	if responseStream != nil {
		responseID := r.id
		model := r.model
		instructions := ""
		store := true
		var previous *string
		if meta != nil {
			if meta.responseID != "" {
				responseID = meta.responseID
			}
			if meta.model != "" {
				model = meta.model
			}
			instructions = meta.instructions
			previous = meta.previous
			store = meta.store
		}
		responseStream <- ResponseStreamEvent{Kind: "response", Response: responseFromTurn(responseID, model, instructions, previous, store, res)}
		if res.FinishReason == "tool_calls" {
			close(responseStream)
		}
	}
}

func (r *turnRunner) emitError(err error) {
	r.updates <- toolproxy.TurnFinalResult{Err: err}
	r.mu.Lock()
	chatStream := r.chatStream
	responseStream := r.responseStream
	r.chatStream = nil
	r.responseStream = nil
	r.responseMeta = nil
	r.mu.Unlock()
	if chatStream != nil {
		chatStream <- StreamEvent{Kind: "error", Error: err}
		close(chatStream)
	}
	if responseStream != nil {
		responseStream <- ResponseStreamEvent{Kind: "error", Error: err}
		close(responseStream)
	}
}

func (r *turnRunner) closeStreams() {
	r.mu.Lock()
	chatStream := r.chatStream
	responseStream := r.responseStream
	r.chatStream = nil
	r.responseStream = nil
	r.responseMeta = nil
	r.mu.Unlock()
	if chatStream != nil {
		close(chatStream)
	}
	if responseStream != nil {
		close(responseStream)
	}
}

func (r *turnRunner) result(text, reasoning string, usage *openai.Usage, finish string) *TurnResult {
	return &TurnResult{ID: r.id, Created: r.created, Model: r.model, SDKSessionID: r.session.SessionID, Text: text, Reasoning: reasoning, Usage: usage, FinishReason: finish, RetainedPath: r.retained}
}

func usageFromSDK(d *copilot.AssistantUsageData) *openai.Usage {
	var prompt, completion, total *int64
	if d.InputTokens != nil {
		v := int64(*d.InputTokens)
		prompt = &v
	}
	if d.OutputTokens != nil {
		v := int64(*d.OutputTokens)
		completion = &v
	}
	if prompt != nil || completion != nil {
		v := int64(0)
		if prompt != nil {
			v += *prompt
		}
		if completion != nil {
			v += *completion
		}
		total = &v
	}
	usage := &openai.Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: total}
	if d.ReasoningTokens != nil {
		v := int64(*d.ReasoningTokens)
		usage.CompletionTokensDetails = &openai.TokenDetails{ReasoningTokens: &v}
	}
	if prompt == nil && completion == nil && usage.CompletionTokensDetails == nil {
		return nil
	}
	return usage
}

func responseFromTurn(id, model, instructions string, previous *string, store bool, turn *TurnResult) *openai.Response {
	if id == "" {
		id = openai.NewID("resp_")
	}
	resp := &openai.Response{ID: id, Object: openai.ObjectResponse, CreatedAt: time.Now().Unix(), Status: "completed", Model: model, Instructions: instructions, Output: []openai.ResponseOutputItem{}, OutputText: "", ParallelToolCalls: true, PreviousResponseID: previous, Store: store, Usage: turn.Usage, Error: nil, IncompleteDetails: nil}
	if len(turn.ToolCalls) > 0 {
		for _, tc := range turn.ToolCalls {
			resp.Output = append(resp.Output, openai.ResponseOutputItem{ID: "fc_" + tc.ID, Type: "function_call", Status: "completed", CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
		return resp
	}
	resp.OutputText = turn.Text
	resp.Output = append(resp.Output, openai.ResponseOutputItem{ID: openai.NewID("msg_"), Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: turn.Text}}})
	return resp
}

func recordFromResponse(resp *openai.Response, sessionID, retained string) sessionstore.ResponseRecord {
	previous := ""
	if resp.PreviousResponseID != nil {
		previous = *resp.PreviousResponseID
	}
	return sessionstore.ResponseRecord{ID: resp.ID, SDKSessionID: sessionID, Model: resp.Model, Instructions: resp.Instructions, CreatedAt: time.Unix(resp.CreatedAt, 0).UTC(), UpdatedAt: time.Now().UTC(), Status: resp.Status, Stored: resp.Store, Output: resp.Output, OutputText: resp.OutputText, Usage: resp.Usage, PreviousResponseID: previous, RetainedPath: retained}
}
