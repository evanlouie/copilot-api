package copilotgw

import (
	"context"
	"fmt"
	"strings"
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

	chatStream      chan<- StreamEvent
	chatDone        <-chan struct{}
	mu              sync.Mutex
	abortOnce       sync.Once
	requestDetached bool
	responseStream  chan<- ResponseStreamEvent
	responseMeta    *responseStreamMeta
	onResult        func(*TurnResult) error
}

type responseStreamMeta struct {
	responseID        string
	model             string
	instructions      string
	previous          *string
	store             bool
	suppressReasoning bool
	done              <-chan struct{}
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
	// Tool-call batches must survive the request that returns the tool_calls
	// response so clients can continue the live SDK session on the next HTTP
	// request. Request cancellation is still enforced by watchContext before the
	// first result, and after a tool-call result the batch TTL owns cleanup.
	rt.SetContext(context.Background())
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
			if r.shouldAbortForRequestContext() {
				r.abort()
			}
		case <-r.closed:
		}
	}()
}

func (r *turnRunner) shouldAbortForRequestContext() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return !r.requestDetached
}

func (r *turnRunner) attachToRequestContext() {
	r.mu.Lock()
	r.requestDetached = false
	r.mu.Unlock()
}

func (r *turnRunner) detachFromRequestContext() {
	r.mu.Lock()
	r.requestDetached = true
	r.mu.Unlock()
}

func (r *turnRunner) abort() {
	r.abortOnce.Do(func() {
		r.rt.CancelCurrent(context.Canceled)
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

func (r *turnRunner) enableResponseStream(ch chan<- ResponseStreamEvent, responseID, model, instructions string, previous *string, store bool, suppressReasoning bool, done <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.responseStream = ch
	if ch == nil {
		r.responseMeta = nil
		return
	}
	r.responseMeta = &responseStreamMeta{responseID: responseID, model: model, instructions: instructions, previous: previous, store: store, suppressReasoning: suppressReasoning, done: done}
}

func (r *turnRunner) loop(g *RealGateway) {
	defer close(r.closed)
	defer r.closeStreams()
	var text string
	var reason reasoningAccumulator
	var usage *openai.Usage
	for event := range r.events {
		switch d := event.Data.(type) {
		case *copilot.AssistantTurnStartData:
			reason.reset()
			text = ""
			usage = nil
		case *copilot.AssistantReasoningDeltaData:
			// Streaming reasoning is dropped by the SDK->wire reduction unless we
			// thread it through here. Accumulate a plaintext fallback and forward
			// the delta so encoders can interleave it ahead of content.
			reason.addDelta(d.DeltaContent, d.ReasoningID)
			if d.DeltaContent != "" {
				r.emitReasoningDelta(d.DeltaContent, d.ReasoningID)
			}
		case *copilot.AssistantMessageDeltaData:
			if d.DeltaContent != "" {
				r.emitDelta(d.DeltaContent)
			}
		case *copilot.AssistantReasoningData:
			// Consolidated reasoning block; in tool-call turns this can arrive
			// after the message. If we already emitted that tool-call turn, do not
			// let its late final block seed the next continuation turn.
			reason.addConsolidated(d.Content, d.ReasoningID)
		case *copilot.AssistantMessageData:
			if d.ReasoningText != nil && *d.ReasoningText != "" {
				reason.consolidated = *d.ReasoningText
			}
			if d.ReasoningOpaque != nil {
				reason.opaque = *d.ReasoningOpaque
			}
			if d.EncryptedContent != nil {
				reason.encrypted = *d.EncryptedContent
			}
			if len(d.ToolRequests) > 0 {
				text = d.Content
				batch, calls := r.rt.CaptureRequests(d.ToolRequests, r.responseID, r.kind, r.model, r.updates, r.abort)
				r.setBatch(batch)
				res := r.result(text, reason.resolve(), usage, "tool_calls")
				reason.applyTo(res)
				res.ToolCalls = calls
				res.PendingBatchID = batch.ID
				r.emitResult(res)
				// The runner loop is reused across the client-owned tool-call
				// continuation, so each tool turn must start a fresh reasoning
				// block. Without this reset the next turn would inherit (or
				// concatenate) this turn's reasoning when its own consolidated
				// block is absent.
				text = ""
				usage = nil
				reason.markToolBoundary()
			} else {
				text = d.Content
			}
		case *copilot.AssistantStreamingDeltaData:
			// Heartbeat carrying only a cumulative byte count; intentionally
			// ignored so it is never mistaken for content.
		case *copilot.AssistantUsageData:
			usage = usageFromSDK(d)
		case *copilot.SessionErrorData:
			err := openai.Upstream(d.Message)
			r.emitError(err)
			_ = r.session.Disconnect()
			return
		case *copilot.SessionIdleData:
			res := r.result(text, reason.resolve(), usage, "stop")
			reason.applyTo(res)
			r.emitResult(res)
			_ = r.session.Disconnect()
			return
		}
	}
	r.emitError(openai.Upstream("copilot session event stream ended before idle"))
}

// reasoningAccumulator gathers the reasoning signals the SDK emits during a
// single assistant turn: streaming deltas, the consolidated block, and the
// opaque/encrypted continuation blobs. The runner loop is reused across the
// client-owned tool-call continuation, so it MUST be reset at each turn
// boundary; otherwise interleaved thinking leaks (or concatenates) between
// turns.
type reasoningAccumulator struct {
	consolidated      string
	deltas            strings.Builder
	opaque            string
	encrypted         string
	id                string
	ignoreLateFinal   bool
	ignoreLateFinalID string
}

func (a *reasoningAccumulator) addDelta(delta, id string) {
	if a.ignoreLateFinal && (a.ignoreLateFinalID == "" || (id != "" && id != a.ignoreLateFinalID)) {
		a.ignoreLateFinal = false
		a.ignoreLateFinalID = ""
	}
	if id != "" {
		a.id = id
	}
	if delta != "" {
		a.deltas.WriteString(delta)
	}
}

func (a *reasoningAccumulator) addConsolidated(content, id string) {
	if a.ignoreLateFinal {
		if a.ignoreLateFinalID == "" || id == "" || id == a.ignoreLateFinalID {
			return
		}
		a.ignoreLateFinal = false
		a.ignoreLateFinalID = ""
	}
	if content != "" {
		a.consolidated = content
	}
	if id != "" {
		a.id = id
	}
}

// resolve returns the best reasoning text for the turn, preferring the
// consolidated block and falling back to the accumulated streaming deltas
// (as happens on tool-call turns where the consolidated block lags).
func (a *reasoningAccumulator) resolve() string {
	return resolveReasoning(a.consolidated, a.deltas.String())
}

// applyTo copies the opaque/encrypted/id continuation fields onto a result.
func (a *reasoningAccumulator) applyTo(res *TurnResult) {
	res.ReasoningOpaque = a.opaque
	res.ReasoningEncrypted = a.encrypted
	res.ReasoningID = a.id
}

// markToolBoundary clears this turn's reasoning after emitting a tool-call
// result, while remembering that the SDK may still send the just-emitted turn's
// final AssistantReasoningData. That late final must not seed the next turn.
func (a *reasoningAccumulator) markToolBoundary() {
	ignoreID := a.id
	a.reset()
	a.ignoreLateFinal = true
	a.ignoreLateFinalID = ignoreID
}

// reset clears all per-turn reasoning state at a turn boundary.
func (a *reasoningAccumulator) reset() {
	a.consolidated = ""
	a.deltas.Reset()
	a.opaque = ""
	a.encrypted = ""
	a.id = ""
	a.ignoreLateFinal = false
	a.ignoreLateFinalID = ""
}

// resolveReasoning prefers the consolidated reasoning text and falls back to
// the accumulated streaming deltas when the SDK has not yet emitted the
// consolidated block (as happens on tool-call turns).
func resolveReasoning(consolidated, deltas string) string {
	if consolidated != "" {
		return consolidated
	}
	return deltas
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

func (r *turnRunner) emitReasoningDelta(delta, reasoningID string) {
	if delta == "" {
		return
	}
	r.mu.Lock()
	chatStream := r.chatStream
	chatDone := r.chatDone
	responseStream := r.responseStream
	meta := r.responseMeta
	r.mu.Unlock()
	if chatStream != nil {
		_ = sendChatStreamEvent(chatStream, chatDone, StreamEvent{Kind: "reasoning_delta", Delta: delta, ReasoningID: reasoningID})
	}
	if responseStream != nil {
		sendResponseStreamEvent(responseStream, meta, ResponseStreamEvent{Kind: "reasoning_delta", Delta: delta, ReasoningID: reasoningID})
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
	r.mu.Lock()
	chatStream := r.chatStream
	chatDone := r.chatDone
	responseStream := r.responseStream
	meta := r.responseMeta
	streaming := chatStream != nil || responseStream != nil
	if res.FinishReason == "tool_calls" {
		r.chatStream = nil
		r.chatDone = nil
		r.responseStream = nil
		r.responseMeta = nil
	}
	r.mu.Unlock()
	if res.FinishReason == "tool_calls" && !streaming {
		// Non-streaming callers receive the parked tool-call result through
		// r.updates; detach before publishing so the handler's deferred request
		// cancellation does not abort the live SDK session needed for continuation.
		r.detachFromRequestContext()
	}
	r.updates <- toolproxy.TurnFinalResult{Value: res}
	if chatStream != nil {
		sent := sendChatStreamEvent(chatStream, chatDone, StreamEvent{Kind: "result", Result: res})
		if res.FinishReason == "tool_calls" && sent {
			r.detachFromRequestContext()
			close(chatStream)
		}
	}
	if responseStream != nil {
		responseID := r.id
		model := r.model
		instructions := ""
		store := true
		suppressReasoning := false
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
			suppressReasoning = meta.suppressReasoning
		}
		sent := sendResponseStreamEvent(responseStream, meta, ResponseStreamEvent{Kind: "response", Response: responseFromTurn(responseID, model, instructions, previous, store, res, suppressReasoning)})
		if res.FinishReason == "tool_calls" && sent {
			r.detachFromRequestContext()
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

func responseFromTurn(id, model, instructions string, previous *string, store bool, turn *TurnResult, suppressReasoning bool) *openai.Response {
	if id == "" {
		id = openai.NewID("resp_")
	}
	resp := &openai.Response{ID: id, Object: openai.ObjectResponse, CreatedAt: time.Now().Unix(), Status: "completed", Model: model, Instructions: instructions, Output: []openai.ResponseOutputItem{}, OutputText: turn.Text, ParallelToolCalls: true, PreviousResponseID: previous, Store: store, Usage: openai.NewResponseUsage(turn.Usage), Error: nil, IncompleteDetails: nil}
	if !suppressReasoning {
		if item, ok := reasoningOutputItem(turn); ok {
			resp.Output = append(resp.Output, item)
		}
	}
	if turn.Text != "" || len(turn.ToolCalls) == 0 {
		resp.Output = append(resp.Output, openai.ResponseOutputItem{ID: openai.NewID("msg_"), Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: turn.Text}}})
	}
	for _, tc := range turn.ToolCalls {
		resp.Output = append(resp.Output, openai.ResponseOutputItem{ID: "fc_" + tc.ID, Type: "function_call", Status: "completed", CallID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return resp
}

// reasoningItemID derives a stable Responses reasoning item ID from the SDK
// reasoning block ID so streamed and final items agree.
func reasoningItemID(turn *TurnResult) string {
	if turn.ReasoningID != "" {
		return "rs_" + turn.ReasoningID
	}
	return openai.NewID("rs_")
}

// reasoningOutputItem builds the Responses `reasoning` output item from a turn,
// carrying the plaintext summary plus any OpenAI-style encrypted continuation
// blob. It reports false when the turn produced no reasoning.
func reasoningOutputItem(turn *TurnResult) (openai.ResponseOutputItem, bool) {
	if turn.Reasoning == "" && turn.ReasoningEncrypted == "" {
		return openai.ResponseOutputItem{}, false
	}
	item := openai.ResponseOutputItem{ID: reasoningItemID(turn), Type: "reasoning", Status: "completed", EncryptedContent: turn.ReasoningEncrypted}
	if turn.Reasoning != "" {
		item.Summary = []openai.ResponseReasoningSummary{{Type: "summary_text", Text: turn.Reasoning}}
	}
	return item, true
}

func recordFromResponse(resp *openai.Response, sessionID, retained string) sessionstore.ResponseRecord {
	previous := ""
	if resp.PreviousResponseID != nil {
		previous = *resp.PreviousResponseID
	}
	return sessionstore.ResponseRecord{ID: resp.ID, SDKSessionID: sessionID, Model: resp.Model, Instructions: resp.Instructions, CreatedAt: time.Unix(resp.CreatedAt, 0).UTC(), UpdatedAt: time.Now().UTC(), Status: resp.Status, Stored: resp.Store, Output: resp.Output, OutputText: resp.OutputText, Usage: resp.Usage, PreviousResponseID: previous, RetainedPath: retained}
}
