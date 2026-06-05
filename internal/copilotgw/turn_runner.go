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
	ctx      context.Context
	session  *copilot.Session
	rt       *toolproxy.RequestTools
	events   <-chan copilot.SessionEvent
	retained string
	kind     string

	responseID string
	created    int64
	batch      *toolproxy.Batch
	updates    chan toolproxy.TurnFinalResult
	closed     chan struct{}

	chatStream     chan<- StreamEvent
	chatDone       <-chan struct{}
	mu             sync.Mutex
	abortOnce      sync.Once
	responseStream chan<- ResponseStreamEvent
	responseMeta   *responseStreamMeta
	onResult       func(*TurnResult) error
}

type responseStreamMeta struct {
	responseID   string
	model        string
	instructions string
	previous     *string
	store        bool
	done         <-chan struct{}
}

func (g *RealGateway) newTurnRunner(ctx context.Context, id, model string, session *copilot.Session, rt *toolproxy.RequestTools, events <-chan copilot.SessionEvent, retained string, kind string, responseID string) *turnRunner {
	if id == "" {
		if kind == "response" {
			id = openai.NewID("resp_")
		} else {
			id = openai.NewID("chatcmpl_")
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rt.SetContext(ctx)
	r := &turnRunner{id: id, model: model, ctx: ctx, session: session, rt: rt, events: events, retained: retained, kind: kind, responseID: responseID, updates: make(chan toolproxy.TurnFinalResult, 16), closed: make(chan struct{}), created: openai.UnixNow()}
	go r.loop(g)
	return r
}

func (r *turnRunner) discardInitial() {
	<-r.updates
}

func (r *turnRunner) watchContext(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			r.abort()
		case <-r.closed:
		}
	}()
}

func (r *turnRunner) abort() {
	r.abortOnce.Do(func() {
		if batch := r.currentBatch(); batch != nil {
			batch.Cancel(context.Canceled)
		}
		_ = r.session.Abort(context.Background())
		_ = r.session.Disconnect()
	})
}

func (r *turnRunner) setBatch(batch *toolproxy.Batch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.batch = batch
}

func (r *turnRunner) currentBatch() *toolproxy.Batch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.batch
}

func (r *turnRunner) setOnResult(fn func(*TurnResult) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onResult = fn
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

func (r *turnRunner) enableChatStream(ch chan<- StreamEvent, done <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chatStream = ch
	r.chatDone = done
}

func (r *turnRunner) enableResponseStream(ch chan<- ResponseStreamEvent, responseID, model, instructions string, previous *string, store bool, done <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.responseStream = ch
	if ch == nil {
		r.responseMeta = nil
		return
	}
	r.responseMeta = &responseStreamMeta{responseID: responseID, model: model, instructions: instructions, previous: previous, store: store, done: done}
}

func (r *turnRunner) loop(g *RealGateway) {
	defer close(r.closed)
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
				batch, calls := r.rt.CaptureRequests(d.ToolRequests, r.responseID, r.kind, r.model, r.updates, r.abort)
				r.setBatch(batch)
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
	chatDone := r.chatDone
	responseStream := r.responseStream
	meta := r.responseMeta
	r.mu.Unlock()
	if chatStream != nil {
		_ = sendChatStreamEvent(chatStream, chatDone, StreamEvent{Kind: "delta", Delta: delta})
	}
	if responseStream != nil {
		sendResponseStreamEvent(responseStream, meta, ResponseStreamEvent{Kind: "delta", Delta: delta})
	}
}

func (r *turnRunner) emitResult(res *TurnResult) {
	r.mu.Lock()
	onResult := r.onResult
	r.mu.Unlock()
	if onResult != nil {
		if err := onResult(res); err != nil {
			r.emitError(err)
			r.abort()
			return
		}
	}
	r.updates <- toolproxy.TurnFinalResult{Value: res}
	r.mu.Lock()
	chatStream := r.chatStream
	chatDone := r.chatDone
	responseStream := r.responseStream
	meta := r.responseMeta
	if res.FinishReason == "tool_calls" {
		r.chatStream = nil
		r.chatDone = nil
		r.responseStream = nil
		r.responseMeta = nil
	}
	r.mu.Unlock()
	if chatStream != nil {
		sent := sendChatStreamEvent(chatStream, chatDone, StreamEvent{Kind: "result", Result: res})
		if res.FinishReason == "tool_calls" && sent {
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
		sent := sendResponseStreamEvent(responseStream, meta, ResponseStreamEvent{Kind: "response", Response: responseFromTurn(responseID, model, instructions, previous, store, res)})
		if res.FinishReason == "tool_calls" && sent {
			close(responseStream)
		}
	}
}

func (r *turnRunner) emitError(err error) {
	r.updates <- toolproxy.TurnFinalResult{Err: err}
	r.mu.Lock()
	chatStream := r.chatStream
	chatDone := r.chatDone
	responseStream := r.responseStream
	meta := r.responseMeta
	r.chatStream = nil
	r.chatDone = nil
	r.responseStream = nil
	r.responseMeta = nil
	r.mu.Unlock()
	if chatStream != nil {
		if sendChatStreamEvent(chatStream, chatDone, StreamEvent{Kind: "error", Error: err}) {
			close(chatStream)
		}
	}
	if responseStream != nil {
		if sendResponseStreamEvent(responseStream, meta, ResponseStreamEvent{Kind: "error", Error: err}) {
			close(responseStream)
		}
	}
}

func sendChatStreamEvent(ch chan<- StreamEvent, done <-chan struct{}, ev StreamEvent) bool {
	if done == nil {
		ch <- ev
		return true
	}
	select {
	case ch <- ev:
		return true
	case <-done:
		return false
	}
}

func sendResponseStreamEvent(ch chan<- ResponseStreamEvent, meta *responseStreamMeta, ev ResponseStreamEvent) bool {
	if meta == nil || meta.done == nil {
		ch <- ev
		return true
	}
	select {
	case ch <- ev:
		return true
	case <-meta.done:
		return false
	}
}

func (r *turnRunner) closeStreams() {
	r.mu.Lock()
	chatStream := r.chatStream
	responseStream := r.responseStream
	r.chatStream = nil
	r.chatDone = nil
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
	resp := &openai.Response{ID: id, Object: openai.ObjectResponse, CreatedAt: time.Now().Unix(), Status: "completed", Model: model, Instructions: instructions, Output: []openai.ResponseOutputItem{}, OutputText: turn.Text, ParallelToolCalls: true, PreviousResponseID: previous, Store: store, Usage: openai.NewResponseUsage(turn.Usage), Error: nil, IncompleteDetails: nil}
	if turn.Text != "" || len(turn.ToolCalls) == 0 {
		resp.Output = append(resp.Output, openai.ResponseOutputItem{ID: openai.NewID("msg_"), Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: turn.Text}}})
	}
	for _, tc := range turn.ToolCalls {
		resp.Output = append(resp.Output, openai.ResponseOutputItem{ID: "fc_" + tc.ID, Type: "function_call", Status: "completed", CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return resp
}

func recordFromResponse(resp *openai.Response, sessionID, retained string) sessionstore.ResponseRecord {
	previous := ""
	if resp.PreviousResponseID != nil {
		previous = *resp.PreviousResponseID
	}
	return sessionstore.ResponseRecord{ID: resp.ID, SDKSessionID: sessionID, Model: resp.Model, Instructions: resp.Instructions, CreatedAt: time.Unix(resp.CreatedAt, 0).UTC(), UpdatedAt: time.Now().UTC(), Status: resp.Status, Stored: resp.Store, Output: resp.Output, OutputText: resp.OutputText, Usage: resp.Usage, PreviousResponseID: previous, RetainedPath: retained}
}
