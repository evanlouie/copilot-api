package copilotgw

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/observability"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
	"github.com/evanlouie/copilot-api/internal/toolproxy"

	copilot "github.com/github/copilot-sdk/go"
)

const (
	// turnRunnerIdleTimeout bounds the gap between copilot session events before
	// the runner loop abandons the turn. It is an unconditional ceiling on runner
	// lifetime because there is no config knob that can serve as one:
	// config.RequestTimeout defaults to 0 (disabled) and only ever covers a single
	// HTTP request, while a runner deliberately outlives its originating request
	// whenever a turn parks on client-owned tool calls. Without an independent
	// bound, a wedged or silently dead SDK session holds a goroutine, an
	// activeRunnerRegistry entry, the client's stream channel and its sessionstore
	// retention pins forever.
	//
	// 15 minutes is far above the gap between events on a healthy turn (the SDK
	// streams deltas continuously, and even a long reasoning stall is seconds),
	// and above the default tool-call TTL so a turn parked on client-owned tool
	// calls is reclaimed by its batch TTL rather than by this ceiling.
	turnRunnerIdleTimeout = 15 * time.Minute
	// turnRunnerIdleToolCallSlack keeps the ceiling strictly above the configured
	// tool-call TTL when an operator raises COPILOT_TOOL_CALL_TTL past
	// turnRunnerIdleTimeout. A parked turn legitimately sees no events until the
	// client returns tool outputs or the batch expires, so the ceiling must never
	// fire before the batch TTL that owns that wait.
	turnRunnerIdleToolCallSlack = time.Minute
	// originatingRequestGeneration is the request generation that owns
	// turnRunner.ctx: the runner is constructed for that request before any
	// attach/detach can happen.
	originatingRequestGeneration uint64 = 0
)

// idleTimeoutForTurns resolves the ceiling the runner loop applies between
// copilot session events.
func (g *RealGateway) idleTimeoutForTurns() time.Duration {
	timeout := turnRunnerIdleTimeout
	if g == nil {
		return timeout
	}
	if floor := g.cfg.ToolCallTTL + turnRunnerIdleToolCallSlack; floor > timeout {
		timeout = floor
	}
	return timeout
}

type turnRunner struct {
	id             string
	model          string
	ctx            context.Context
	session        copilotSession
	rt             *toolproxy.RequestTools
	events         <-chan copilot.SessionEvent
	retained       string
	kind           string
	maxOutputBytes int64
	idleTimeout    time.Duration

	responseID string
	created    int64
	batch      *toolproxy.Batch
	updates    chan toolproxy.TurnFinalResult
	closed     chan struct{}
	// aborted is closed by abort so the loop observes a self-inflicted abort.
	// session.Abort plus Disconnect stops event delivery for good, so waiting on
	// the event channel alone would park the loop - and every cleanup it owns -
	// forever. A nil channel simply never fires, which keeps runners built
	// directly in tests usable.
	aborted chan struct{}

	chat              streamSink[StreamEvent]
	mu                sync.Mutex
	abortOnce         sync.Once
	abortSignalOnce   sync.Once
	requestDetached   bool
	requestGeneration uint64
	response          streamSink[ResponseStreamEvent]
	responseParams    *responseParams
	// messageItemID, reasoningItemID and itemOrder are this turn's output-item
	// identity. The runner assigns each ID once, hands it to the client on the
	// first delta that belongs to the item, and reuses it when it builds the
	// terminal response - so no other layer ever has to invent one. They are
	// per-turn state and are cleared at every turn boundary.
	messageItemID   string
	reasoningItemID string
	itemOrder       []string
	onResult        func(*TurnResult) error
	store           *sessionstore.Store
	pinMu           sync.Mutex
	pinReleases     []func()
	pinsReleased    bool
}

// responseParams is the request-scoped identity of the response a turn
// produces. It is set before the turn can complete and is the only input
// responseFromTurn takes besides the turn itself.
type responseParams struct {
	id           string
	created      int64
	model        string
	instructions string
	previous     *string
	store        bool
	metadata     map[string]string
}

// newTurnRunner builds a runner and starts the loop that owns it, or fails.
//
// It returns an error rather than a runner when the gateway is shutting down.
// loop is the sole producer on r.updates and the sole owner of closeStreams,
// the active-registry entry and the retention pins, so a runner handed back
// without one is not a degraded runner - it is a request that can never
// complete: waitInitial blocks until the request context dies, the stream
// channel is never closed, and discardInitial parks on <-r.updates forever.
// Callers must fail the request instead.
func (g *RealGateway) newTurnRunner(ctx context.Context, id, model string, session copilotSession, rt *toolproxy.RequestTools, events *sessionEventSink, retained string, kind string, responseID string) (*turnRunner, error) {
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
	maxOutputBytes := g.cfg.MaxTurnOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = config.DefaultMaxTurnOutputBytes
	}
	r := &turnRunner{id: id, model: model, ctx: ctx, session: session, rt: rt, events: events.events(), retained: retained, kind: kind, maxOutputBytes: maxOutputBytes, idleTimeout: g.idleTimeoutForTurns(), responseID: responseID, updates: make(chan toolproxy.TurnFinalResult, 16), closed: make(chan struct{}), aborted: make(chan struct{}), created: openai.UnixNow(), store: g.store}
	// The loop is this sink's only reader, so the sink needs its liveness signal
	// to stop waiting on a runner that has finished (or never started).
	events.attach(r.closed)
	if g.store != nil && session != nil {
		r.addPin(g.store.PinSession(session.ID()))
	}
	if kind == "response" && responseID != "" && g.store != nil {
		r.addPin(g.store.PinResponse(responseID))
	}
	if g.active == nil {
		g.active = newActiveRunnerRegistry()
	}
	if !g.active.add(r) {
		// Stop has already snapshotted the registry, so no loop started here would
		// ever be awaited. Tear down what this call took ownership of - the SDK
		// session, the tool-call state and the pins added above - and decline. A
		// shutdown is this service refusing the request, not a dependency failing,
		// which is the same 503 WarmResponse returns for the equivalent race.
		r.abort()
		r.releasePins()
		close(r.closed)
		return nil, apierr.Unavailable("gateway is shutting down")
	}
	go r.loop(g)
	return r, nil
}

// discardInitial drains the first turn result for callers that stream instead
// of waiting on it. It also stops when the runner closes, so it can never
// outlive the loop that is its only producer.
func (r *turnRunner) discardInitial() {
	select {
	case <-r.updates:
	case <-r.closed:
	}
}

func (r *turnRunner) watchContext(ctx context.Context) {
	r.mu.Lock()
	generation := r.requestGeneration
	r.mu.Unlock()
	go func() {
		select {
		case <-ctx.Done():
			if r.shouldAbortForRequestGeneration(generation) {
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

func (r *turnRunner) shouldAbortForRequestGeneration(generation uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return generation == r.requestGeneration && !r.requestDetached
}

func (r *turnRunner) attachToRequestContext() {
	r.mu.Lock()
	r.requestGeneration++
	r.requestDetached = false
	r.mu.Unlock()
}

func (r *turnRunner) detachFromRequestContext() {
	r.mu.Lock()
	r.requestDetached = true
	r.mu.Unlock()
}

func (r *turnRunner) abort() {
	// Release the loop before touching the SDK: Abort and Disconnect are blocking
	// RPCs, and the loop must not depend on them returning to reach its cleanup.
	r.signalAbort()
	r.abortOnce.Do(func() {
		r.rt.CancelCurrent(context.Canceled)
		if batch := r.currentBatch(); batch != nil {
			batch.Cancel(context.Canceled)
		}
		_ = r.session.Abort(context.Background())
		_ = r.session.Disconnect()
	})
}

// signalAbort closes the loop's termination signal exactly once. Nothing else
// tells the loop that an aborted turn is over: session.Abort followed by
// Disconnect stops event delivery, so the event channel would simply go quiet.
func (r *turnRunner) signalAbort() {
	if r.aborted == nil {
		return
	}
	r.abortSignalOnce.Do(func() { close(r.aborted) })
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

// currentResponseID follows active continuation metadata so a reused runner
// parks tool calls under the continuation response id, not the id from the
// original request that created the SDK session.
func (r *turnRunner) currentResponseID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.responseParams != nil && r.responseParams.id != "" {
		return r.responseParams.id
	}
	return r.responseID
}

func (r *turnRunner) setCurrentResponseID(id string) {
	if id == "" {
		return
	}
	r.mu.Lock()
	changed := r.responseID != id
	r.id = id
	r.responseID = id
	r.mu.Unlock()
	if changed && r.store != nil {
		r.addPin(r.store.PinResponse(id))
	}
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
			return nil, apierr.Internal(fmt.Sprintf("unexpected turn result %T", first.Value))
		}
		return res, nil
	case <-ctx.Done():
		return nil, requestContextError(ctx)
	}
}

func requestContextError(ctx context.Context) error {
	if ctx != nil && ctx.Err() == context.DeadlineExceeded {
		return apierr.Timeout()
	}
	return context.Canceled
}

func (r *turnRunner) enableChatStream(ch chan<- StreamEvent, done <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.chat.attach(ch, done)
}

func (r *turnRunner) enableResponseStream(ch chan<- ResponseStreamEvent, done <-chan struct{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.response.attach(ch, done)
}

// setResponseParams records the request-scoped identity of the response the
// next turn produces. Both the streaming and non-streaming Responses paths set
// it, which is what lets the runner - and only the runner - build that
// response.
func (r *turnRunner) setResponseParams(params responseParams) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.responseParams = &params
}

// resetOutputItems clears the per-turn output-item identity at a turn boundary.
// The runner loop is reused across a client-owned tool-call continuation, so a
// continuation turn must not inherit the emitted turn's item IDs or ordering.
func (r *turnRunner) resetOutputItems() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messageItemID = ""
	r.reasoningItemID = ""
	r.itemOrder = nil
}

// announceItemLocked records that an output item has been published on the
// response stream. The order of first announcement is the single source of
// truth for output_index: responseFromTurn arranges the terminal output to
// match it.
func (r *turnRunner) announceItemLocked(id string) {
	if id == "" || !r.response.active() {
		return
	}
	for _, seen := range r.itemOrder {
		if seen == id {
			return
		}
	}
	r.itemOrder = append(r.itemOrder, id)
}

// messageItemIDLocked assigns this turn's message output-item ID on first use.
func (r *turnRunner) messageItemIDLocked() string {
	if r.messageItemID == "" {
		r.messageItemID = openai.NewID("msg_")
	}
	return r.messageItemID
}

// reasoningItemIDLocked assigns this turn's reasoning output-item ID on first
// use, preferring the SDK reasoning block ID so the item is stable across a
// resumed session. First assignment wins: a later block ID must not rename an
// item the client has already seen.
func (r *turnRunner) reasoningItemIDLocked(reasoningID string) string {
	if r.reasoningItemID == "" {
		if reasoningID != "" {
			r.reasoningItemID = "rs_" + reasoningID
		} else {
			r.reasoningItemID = openai.NewID("rs_")
		}
	}
	return r.reasoningItemID
}

// loop owns the runner's lifetime. It is the sole owner of every cleanup the
// rest of the gateway waits on - the closed signal (RealGateway.Stop and the
// event sink), the active-registry entry, the client stream channels and the
// sessionstore retention pins - so it must terminate on every path. Each wait
// it performs is therefore bounded: by the event stream, by a self-inflicted
// abort, by the originating request's cancellation, or by the idle ceiling.
func (r *turnRunner) loop(g *RealGateway) {
	defer close(r.closed)
	if g != nil && g.active != nil {
		defer g.active.remove(r)
	}
	defer r.closeStreams()
	defer r.releasePins()
	// A turn can contain more than one assistant message (the SDK identifies them
	// with AssistantMessageData.MessageID), and every message's deltas are
	// forwarded to the client. The terminal text therefore has to accumulate the
	// messages instead of holding only the last one, or the result would disagree
	// with what the client was already shown. Its lifetime matches the reasoning
	// accumulator's: cleared at the turn start and at the tool-call boundary.
	var text strings.Builder
	var reason reasoningAccumulator
	// toolCalls routes incremental tool-call argument fragments to the client.
	// Like text and reasoning it is per-turn state: a continuation turn plans its
	// own calls and must not inherit the emitted turn's routing.
	toolCalls := toolCallStreamState{}
	var usage *openai.Usage
	var contentBytes int64
	var reasoningStreamBytes int64
	var toolCallStreamBytes int64
	stats := newTurnDebugStats()
	debugEnabled := g != nil && g.log != nil && g.log.Enabled(r.ctx, slog.LevelDebug)
	idleTimeout := r.idleTimeout
	if idleTimeout <= 0 {
		idleTimeout = turnRunnerIdleTimeout
	}
	idle := time.NewTimer(idleTimeout)
	defer idle.Stop()
	// requestDone is the originating request's cancellation, which is only ours to
	// act on while this runner still belongs to that request. r.ctx is generation
	// zero by construction: attachToRequestContext only ever moves the generation
	// forward, and a turn that parks on client-owned tool calls detaches so the
	// follow-up request can re-attach with its own context and watchContext.
	// Capturing the generation here instead would be wrong, because a re-attached
	// runner would then treat the long-gone original context as its own.
	var requestDone <-chan struct{}
	if r.ctx != nil {
		requestDone = r.ctx.Done()
	}
	for {
		select {
		case event, ok := <-r.events:
			if !ok {
				r.debug(g, "copilot session event stream ended before idle")
				r.emitError(apierr.Upstream("copilot session event stream ended before idle"))
				return
			}
			// Only a delivered event proves the session is still alive, so the
			// ceiling measures the gap between events rather than turn duration.
			// Go 1.23+ timers leave no stale value behind Stop, so no drain here.
			idle.Stop()
			idle.Reset(idleTimeout)
			switch d := event.Data.(type) {
			case *copilot.AssistantTurnStartData:
				reason.reset()
				text.Reset()
				r.resetOutputItems()
				usage = nil
				contentBytes = 0
				reasoningStreamBytes = 0
				toolCallStreamBytes = 0
				clear(toolCalls)
				stats.reset()
				r.debug(g, "copilot turn started")
			case *copilot.AssistantMessageStartData:
				r.debug(g, "copilot assistant message started", "message_id", d.MessageID, "phase", optionalString(d.Phase), "ms_since_turn_start", stats.msSinceTurnStart())
			case *copilot.AssistantReasoningDeltaData:
				if contentBytes+reasoningStreamBytes+int64(len(d.DeltaContent)) > r.maxOutputBytes || contentBytes+reason.retainedSizeAfterDelta(d.DeltaContent) > r.maxOutputBytes {
					r.emitError(apierr.Upstream("copilot reasoning output exceeded size limit"))
					r.abort()
					return
				}
				// Streaming reasoning is dropped by the SDK->wire reduction unless we
				// thread it through here. Accumulate a plaintext fallback and forward
				// the delta so encoders can interleave it ahead of content.
				reason.addDelta(d.DeltaContent, d.ReasoningID)
				reasoningStreamBytes += int64(len(d.DeltaContent))
				if d.DeltaContent != "" {
					if debugEnabled {
						deltaStats := stats.observeReasoningDelta(d.DeltaContent)
						r.debugDelta(g, "copilot reasoning delta", d.DeltaContent, deltaStats, "reasoning_id", d.ReasoningID)
					}
					r.emitReasoningDelta(d.DeltaContent, d.ReasoningID)
				}
			case *copilot.AssistantMessageDeltaData:
				if d.DeltaContent != "" {
					if contentBytes+int64(len(d.DeltaContent))+reasoningStreamBytes > r.maxOutputBytes || contentBytes+int64(len(d.DeltaContent))+reason.retainedSize() > r.maxOutputBytes {
						r.emitError(apierr.Upstream("copilot output exceeded size limit"))
						r.abort()
						return
					}
					contentBytes += int64(len(d.DeltaContent))
					if debugEnabled {
						deltaStats := stats.observeContentDelta(d.DeltaContent)
						r.debugDelta(g, "copilot content delta", d.DeltaContent, deltaStats, "message_id", d.MessageID)
					}
					r.emitDelta(d.DeltaContent)
				}
			case *copilot.AssistantToolCallDeltaData:
				// The SDK streams a tool call's provider input in fragments well
				// before the finished tool request arrives, which is the only chance
				// a client has to render arguments as they are produced.
				if d.InputDelta != "" {
					toolCallStreamBytes += int64(len(d.InputDelta))
					if contentBytes+reasoningStreamBytes+toolCallStreamBytes > r.maxOutputBytes {
						r.emitError(apierr.Upstream("copilot tool-call output exceeded size limit"))
						r.abort()
						return
					}
					stream := toolCalls.resolve(r.rt, d)
					if debugEnabled {
						r.debug(g, "copilot tool call delta", "tool_call_id", d.ToolCallID, "tool_name", optionalString(d.ToolName), "delta_bytes", len(d.InputDelta), "forwarded", stream.forwarded(), "ms_since_turn_start", stats.msSinceTurnStart())
					}
					r.emitToolCallDelta(stream, d.InputDelta)
				}
			case *copilot.AssistantReasoningData:
				if contentBytes+reason.retainedSizeAfterConsolidated(d.Content) > r.maxOutputBytes {
					r.emitError(apierr.Upstream("copilot reasoning output exceeded size limit"))
					r.abort()
					return
				}
				// Consolidated reasoning block; in tool-call turns this can arrive
				// after the message. If we already emitted that tool-call turn, do not
				// let its late final block seed the next continuation turn.
				reason.addConsolidated(d.Content, d.ReasoningID)
				if debugEnabled {
					r.debug(g, "copilot final reasoning block", "reasoning_id", d.ReasoningID, "content_bytes", len(d.Content), "content_runes", utf8.RuneCountInString(d.Content), "ms_since_turn_start", stats.msSinceTurnStart())
				}
			case *copilot.AssistantMessageData:
				toolRequestBytes, err := toolRequestPayloadSize(d.ToolRequests)
				if err != nil {
					r.emitError(apierr.Upstream("failed to measure copilot tool-call output"))
					r.abort()
					return
				}
				reasoningText := reason.resolve()
				if d.ReasoningText != nil && *d.ReasoningText != "" {
					reasoningText = *d.ReasoningText
				}
				reasoningBytes := len(reasoningText) + optionalStringByteLen(d.ReasoningOpaque) + optionalStringByteLen(d.EncryptedContent)
				// text already holds this turn's earlier messages, so the guard has to
				// measure the whole retained turn rather than this message alone.
				if int64(text.Len()+len(d.Content)+reasoningBytes)+toolRequestBytes > r.maxOutputBytes {
					r.emitError(apierr.Upstream("copilot output exceeded size limit"))
					r.abort()
					return
				}
				if d.ReasoningText != nil && *d.ReasoningText != "" {
					reason.consolidated = *d.ReasoningText
					reason.deltas = strings.Builder{}
				}
				if d.ReasoningOpaque != nil {
					reason.opaque = *d.ReasoningOpaque
				}
				if d.EncryptedContent != nil {
					reason.encrypted = *d.EncryptedContent
				}
				if debugEnabled {
					r.debug(g, "copilot final assistant message", append([]any{"message_id", d.MessageID, "content_bytes", len(d.Content), "content_runes", utf8.RuneCountInString(d.Content), "reasoning_text_bytes", optionalStringByteLen(d.ReasoningText), "tool_request_count", len(d.ToolRequests)}, stats.summaryAttrs()...)...)
				}
				if len(d.ToolRequests) > 0 {
					text.WriteString(d.Content)
					batch, calls, err := r.rt.CaptureRequests(d.ToolRequests, r.currentResponseID(), r.kind, r.model, r.updates, r.abort)
					if err != nil {
						r.emitError(apierr.Upstream(err.Error()))
						r.abort()
						return
					}
					r.setBatch(batch)
					res := r.result(text.String(), reason.resolve(), usage, "tool_calls")
					reason.applyTo(res)
					res.ResponseToolCalls = calls
					res.ToolCalls = chatToolCallsFromCaptured(calls)
					res.PendingBatchID = batch.ID
					if r.emitResult(res) {
						return
					}
					// The runner loop is reused across the client-owned tool-call
					// continuation, so each tool turn must start a fresh reasoning
					// block. Without this reset the next turn would inherit (or
					// concatenate) this turn's reasoning when its own consolidated
					// block is absent. The same applies to the assistant text and to
					// the turn's output-item identity: the continuation turn must not
					// inherit the emitted turn's messages or item IDs.
					text.Reset()
					usage = nil
					contentBytes = 0
					reasoningStreamBytes = 0
					toolCallStreamBytes = 0
					clear(toolCalls)
					reason.markToolBoundary()
					r.resetOutputItems()
					stats.reset()
				} else {
					text.WriteString(d.Content)
				}
			case *copilot.AssistantStreamingDeltaData:
				stats.observeStreamProgress(d.TotalResponseSizeBytes)
				r.debug(g, "copilot stream progress", "total_response_size_bytes", d.TotalResponseSizeBytes, "stream_progress_count", stats.streamProgressCount, "ms_since_turn_start", stats.msSinceTurnStart())
			case *copilot.AssistantUsageData:
				usage = usageFromSDK(d)
				r.debug(g, "copilot usage received", "input_tokens", optionalInt(d.InputTokens), "output_tokens", optionalInt(d.OutputTokens), "reasoning_tokens", optionalInt(d.ReasoningTokens), "ms_since_turn_start", stats.msSinceTurnStart())
			case *copilot.SessionErrorData:
				err := upstreamSessionError(d)
				r.debug(g, "copilot session error", "error", d.Message, "error_type", d.ErrorType, "kind", string(err.Kind), "ms_since_turn_start", stats.msSinceTurnStart())
				r.emitError(err)
				_ = r.session.Disconnect()
				return
			case *copilot.SessionIdleData:
				res := r.result(text.String(), reason.resolve(), usage, "stop")
				reason.applyTo(res)
				if debugEnabled {
					r.debug(g, "copilot session idle", append([]any{"finish_reason", res.FinishReason, "final_text_bytes", len(res.Text), "final_text_runes", utf8.RuneCountInString(res.Text), "final_reasoning_bytes", len(res.Reasoning)}, stats.summaryAttrs()...)...)
				}
				r.emitResult(res)
				_ = r.session.Disconnect()
				return
			}
		case <-requestDone:
			if !r.shouldAbortForRequestGeneration(originatingRequestGeneration) {
				// The turn either parked on client-owned tool calls (detached) or a
				// later request re-attached to this runner. Either way the originating
				// request's cancellation is not ours to act on, and because attaching
				// only ever moves the generation forward it can never become ours
				// again: stop selecting on an already-closed channel.
				requestDone = nil
				continue
			}
			r.debug(g, "copilot turn cancelled by the originating request", "ms_since_turn_start", stats.msSinceTurnStart())
			r.abort()
			r.emitError(requestContextError(r.ctx))
			return
		case <-r.aborted:
			// Abort stops event delivery, so this is the only signal that the turn
			// is over. It covers gateway shutdown, tool-call batch expiry, and
			// aborts this loop inflicted on itself.
			r.debug(g, "copilot turn aborted before completion", "ms_since_turn_start", stats.msSinceTurnStart())
			r.emitError(apierr.Upstream("copilot turn aborted before completion"))
			return
		case <-idle.C:
			r.debug(g, "copilot session idle timeout", "idle_timeout", idleTimeout.String(), "ms_since_turn_start", stats.msSinceTurnStart())
			r.abort()
			r.emitError(apierr.Upstream(fmt.Sprintf("copilot session delivered no events for %s; turn abandoned", idleTimeout)))
			return
		}
	}
}

type turnDebugStats struct {
	turnStarted             time.Time
	lastDelta               time.Time
	contentDeltaCount       int
	contentDeltaBytes       int
	maxContentDeltaBytes    int
	reasoningDeltaCount     int
	reasoningDeltaBytes     int
	maxReasoningDeltaBytes  int
	streamProgressCount     int
	lastStreamProgressBytes int64
}

type deltaDebugStats struct {
	index            int
	cumulativeBytes  int
	maxBytes         int
	msSinceTurnStart int64
	msSincePrevDelta int64
}

func newTurnDebugStats() *turnDebugStats {
	s := &turnDebugStats{}
	s.reset()
	return s
}

func (s *turnDebugStats) reset() {
	*s = turnDebugStats{turnStarted: time.Now()}
}

func (s *turnDebugStats) observeContentDelta(delta string) deltaDebugStats {
	now := time.Now()
	gap := elapsedMillisSince(s.lastDelta, now)
	s.lastDelta = now
	s.contentDeltaCount++
	s.contentDeltaBytes += len(delta)
	if len(delta) > s.maxContentDeltaBytes {
		s.maxContentDeltaBytes = len(delta)
	}
	return deltaDebugStats{index: s.contentDeltaCount, cumulativeBytes: s.contentDeltaBytes, maxBytes: s.maxContentDeltaBytes, msSinceTurnStart: elapsedMillisSince(s.turnStarted, now), msSincePrevDelta: gap}
}

func (s *turnDebugStats) observeReasoningDelta(delta string) deltaDebugStats {
	now := time.Now()
	gap := elapsedMillisSince(s.lastDelta, now)
	s.lastDelta = now
	s.reasoningDeltaCount++
	s.reasoningDeltaBytes += len(delta)
	if len(delta) > s.maxReasoningDeltaBytes {
		s.maxReasoningDeltaBytes = len(delta)
	}
	return deltaDebugStats{index: s.reasoningDeltaCount, cumulativeBytes: s.reasoningDeltaBytes, maxBytes: s.maxReasoningDeltaBytes, msSinceTurnStart: elapsedMillisSince(s.turnStarted, now), msSincePrevDelta: gap}
}

func (s *turnDebugStats) observeStreamProgress(total int64) {
	s.streamProgressCount++
	s.lastStreamProgressBytes = total
}

func (s *turnDebugStats) msSinceTurnStart() int64 {
	return elapsedMillisSince(s.turnStarted, time.Now())
}

// summaryAttrs returns the cumulative per-turn streaming metrics shared by the
// end-of-turn debug logs (final assistant message and session idle).
func (s *turnDebugStats) summaryAttrs() []any {
	return []any{
		"content_delta_count", s.contentDeltaCount,
		"content_delta_bytes", s.contentDeltaBytes,
		"max_content_delta_bytes", s.maxContentDeltaBytes,
		"reasoning_delta_count", s.reasoningDeltaCount,
		"reasoning_delta_bytes", s.reasoningDeltaBytes,
		"max_reasoning_delta_bytes", s.maxReasoningDeltaBytes,
		"stream_progress_count", s.streamProgressCount,
		"last_stream_progress_bytes", s.lastStreamProgressBytes,
		"ms_since_turn_start", s.msSinceTurnStart(),
	}
}

func elapsedMillisSince(start, end time.Time) int64 {
	if start.IsZero() {
		return -1
	}
	return end.Sub(start).Milliseconds()
}

func (r *turnRunner) debug(g *RealGateway, msg string, attrs ...any) {
	if g == nil || g.log == nil || !g.log.Enabled(r.ctx, slog.LevelDebug) {
		return
	}
	r.mu.Lock()
	id := r.id
	r.mu.Unlock()
	base := []any{"session_id", r.sessionID(), "openai_id", id, "stream_kind", r.kind, "model", r.model}
	base = append(base, attrs...)
	observability.Logger(r.ctx, g.log).DebugContext(r.ctx, msg, base...)
}

func (r *turnRunner) debugDelta(g *RealGateway, msg, delta string, stats deltaDebugStats, attrs ...any) {
	if g == nil || g.log == nil || !g.log.Enabled(r.ctx, slog.LevelDebug) {
		return
	}
	r.mu.Lock()
	id := r.id
	r.mu.Unlock()
	base := []any{
		"session_id", r.sessionID(),
		"openai_id", id,
		"stream_kind", r.kind,
		"model", r.model,
		"delta_index", stats.index,
		"delta_bytes", len(delta),
		"delta_runes", utf8.RuneCountInString(delta),
		"cumulative_delta_bytes", stats.cumulativeBytes,
		"max_delta_bytes", stats.maxBytes,
		"ms_since_turn_start", stats.msSinceTurnStart,
		"ms_since_previous_delta", stats.msSincePrevDelta,
	}
	if g.cfg.LogContent {
		base = append(base, "delta_preview", observability.TruncateForLog(delta, 160))
	}
	base = append(base, attrs...)
	observability.Logger(r.ctx, g.log).DebugContext(r.ctx, msg, base...)
}

func (r *turnRunner) sessionID() string {
	if r.session == nil {
		return ""
	}
	return r.session.ID()
}

type byteCounter int64

func (c *byteCounter) Write(p []byte) (int, error) {
	*c += byteCounter(len(p))
	return len(p), nil
}

func toolRequestPayloadSize(requests []copilot.AssistantMessageToolRequest) (int64, error) {
	if len(requests) == 0 {
		return 0, nil
	}
	var counter byteCounter
	if err := json.NewEncoder(&counter).Encode(requests); err != nil {
		return 0, err
	}
	return int64(counter), nil
}

func optionalString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func optionalStringByteLen(v *string) int {
	if v == nil {
		return 0
	}
	return len(*v)
}

func optionalInt(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
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

func (a *reasoningAccumulator) retainedSize() int64 {
	textBytes := len(a.consolidated)
	if textBytes == 0 {
		textBytes = a.deltas.Len()
	}
	return int64(textBytes + len(a.opaque) + len(a.encrypted))
}

func (a *reasoningAccumulator) retainedSizeAfterDelta(delta string) int64 {
	textBytes := len(a.consolidated)
	if textBytes == 0 {
		textBytes = a.deltas.Len() + len(delta)
	}
	return int64(textBytes + len(a.opaque) + len(a.encrypted))
}

func (a *reasoningAccumulator) retainedSizeAfterConsolidated(content string) int64 {
	return int64(len(content) + len(a.opaque) + len(a.encrypted))
}

func (a *reasoningAccumulator) addDelta(delta, id string) {
	if a.ignoreLateFinal && (a.ignoreLateFinalID == "" || (id != "" && id != a.ignoreLateFinalID)) {
		a.ignoreLateFinal = false
		a.ignoreLateFinalID = ""
	}
	if id != "" {
		a.id = id
	}
	if delta != "" && a.consolidated == "" {
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
		a.deltas = strings.Builder{}
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
	a.deltas = strings.Builder{}
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
	chat := r.chat
	response := r.response
	var itemID string
	if response.active() {
		itemID = r.messageItemIDLocked()
		r.announceItemLocked(itemID)
	}
	r.mu.Unlock()
	chat.send(StreamEvent{Kind: "delta", Delta: delta})
	response.send(ResponseStreamEvent{Kind: "delta", ItemID: itemID, Delta: delta})
}

// toolCallStreamState is the current turn's routing table for incremental
// tool-call arguments, keyed by the model's tool-call id.
type toolCallStreamState map[string]*toolCallStream

// toolCallStream is one tool call's incremental-argument routing: the identity
// its fragments are published under, and the item they extend.
//
// A zero value is the decision not to forward this call's fragments at all,
// which is how a strict tool, an unrecognised tool, a freeform custom tool and
// a tool-search call are all represented (see
// toolproxy.RequestTools.ReserveStreamingCall for why each is excluded). Once
// made, that decision is remembered for the rest of the turn: a later fragment
// must not start a stream halfway through a call's arguments, since the client
// would then hold a fragment it cannot place.
type toolCallStream struct {
	call toolproxy.StreamingCall
	// item is the in-progress Responses output item the fragments extend.
	item *openai.ResponseOutputItem
}

func (s *toolCallStream) forwarded() bool { return s != nil && s.call.CallID != "" }

// resolve classifies a tool call the first time a fragment for it arrives.
//
// The tool name is what resolves the call back to a declared tool, and
// therefore what decides whether it is strict. A fragment that arrives without
// one cannot be classified, so it is buffered rather than guessed at.
func (s toolCallStreamState) resolve(rt *toolproxy.RequestTools, d *copilot.AssistantToolCallDeltaData) *toolCallStream {
	if stream, ok := s[d.ToolCallID]; ok {
		return stream
	}
	custom := d.ToolType != nil && *d.ToolType == copilot.AssistantMessageToolRequestTypeCustom
	stream := &toolCallStream{}
	if call, ok := rt.ReserveStreamingCall(d.ToolCallID, optionalString(d.ToolName), custom); ok {
		stream = newToolCallStream(call)
	}
	s[d.ToolCallID] = stream
	return stream
}

func newToolCallStream(call toolproxy.StreamingCall) *toolCallStream {
	// ReserveStreamingCall admits nothing but function calls, so this is the only
	// item shape the fragments can extend. Checking rather than assuming keeps a
	// later widening of that gate from silently routing another kind's fragments
	// into a function_call item.
	if call.Kind != toolcatalog.ToolKindFunction {
		return &toolCallStream{}
	}
	item := &openai.ResponseOutputItem{ID: responseToolItemID(call.Kind, call.CallID), Type: "function_call", Status: "in_progress", CallID: call.CallID, Namespace: call.Namespace, Name: call.Name}
	return &toolCallStream{call: call, item: item}
}

// emitToolCallDelta publishes one tool-call argument fragment. The item it
// names is announced here for the same reason a message item is: the order of
// first announcement is what responseFromTurn arranges the terminal output to
// match.
func (r *turnRunner) emitToolCallDelta(stream *toolCallStream, delta string) {
	if !stream.forwarded() || delta == "" {
		return
	}
	r.mu.Lock()
	chat := r.chat
	response := r.response
	r.announceItemLocked(stream.item.ID)
	r.mu.Unlock()
	chat.send(StreamEvent{Kind: "tool_call_delta", Delta: delta, ToolCallID: stream.call.CallID, ToolName: stream.call.Name})
	response.send(ResponseStreamEvent{Kind: "tool_call_delta", ItemID: stream.item.ID, Item: stream.item, Delta: delta})
}

func (r *turnRunner) emitReasoningDelta(delta, reasoningID string) {
	if delta == "" {
		return
	}
	r.mu.Lock()
	chat := r.chat
	response := r.response
	var itemID string
	if response.active() {
		itemID = r.reasoningItemIDLocked(reasoningID)
		r.announceItemLocked(itemID)
	}
	r.mu.Unlock()
	chat.send(StreamEvent{Kind: "reasoning_delta", Delta: delta, ReasoningID: reasoningID})
	response.send(ResponseStreamEvent{Kind: "reasoning_delta", ItemID: itemID, Delta: delta})
}

// emitResult publishes a turn result and reports whether the runner loop must
// stop. The persistence callback can fail (a SaveResponse or disk error), and
// that failure aborts the SDK session: no further events will ever arrive, so
// the loop has to exit rather than wait for them.
func (r *turnRunner) emitResult(res *TurnResult) (stop bool) {
	// Stamp the turn's output-item identity and build its response before any
	// consumer can observe the result. Persistence, the streamed terminal event
	// and the non-streaming JSON body then all read TurnResult.Response, so a
	// single construction serves every transport and the store.
	r.stampOutputItems(res)
	r.buildTurnResponse(res)
	r.mu.Lock()
	// Persistence behavior belongs to exactly one model turn. Taking and
	// clearing it prevents a streamed tool-call turn's callback from being
	// reused by a later non-streaming continuation on the same runner.
	onResult := r.onResult
	r.onResult = nil
	r.mu.Unlock()
	if onResult != nil {
		if err := onResult(res); err != nil {
			r.emitError(err)
			r.abort()
			return true
		}
	}
	r.mu.Lock()
	chat := r.chat
	response := r.response
	if res.FinishReason == "tool_calls" {
		r.chat.attach(nil, nil)
		r.response.attach(nil, nil)
	}
	r.mu.Unlock()
	if res.FinishReason == "tool_calls" {
		// Once a tool-call result exists, its batch TTL owns liveness. Detach
		// before publishing to any transport so cancellation cannot win the narrow
		// interval between result delivery and detachment.
		r.detachFromRequestContext()
	}
	r.updates <- toolproxy.TurnFinalResult{Value: res}
	if chat.send(StreamEvent{Kind: "result", Result: res}) && res.FinishReason == "tool_calls" {
		chat.close()
	}
	if response.send(ResponseStreamEvent{Kind: "response", Response: res.Response}) && res.FinishReason == "tool_calls" {
		response.close()
	}
	return false
}

// stampOutputItems copies this turn's output-item identity onto the result:
// the IDs the client has already seen on streamed deltas, plus the order they
// were announced in. It never mints an ID - a turn that streamed nothing gets
// its IDs from responseFromTurn, and a chat turn never needs any.
func (r *turnRunner) stampOutputItems(res *TurnResult) {
	if res == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if res.MessageItemID == "" {
		res.MessageItemID = r.messageItemID
	}
	if res.ReasoningItemID == "" {
		res.ReasoningItemID = r.reasoningItemID
	}
	if res.itemOrder == nil {
		res.itemOrder = append([]string(nil), r.itemOrder...)
	}
}

// buildTurnResponse builds the turn's one and only openai.Response and caches
// it on the result. It is the sole call site of responseFromTurn on the live
// path; every consumer reads TurnResult.Response afterwards.
func (r *turnRunner) buildTurnResponse(res *TurnResult) {
	if res == nil || res.Response != nil {
		return
	}
	r.mu.Lock()
	params := r.responseParams
	streaming := r.response.active()
	model := r.model
	created := r.created
	r.mu.Unlock()
	if params == nil && !streaming {
		return
	}
	p := responseParams{store: true}
	if params != nil {
		p = *params
	}
	if p.id == "" {
		p.id = res.ID
	}
	if p.model == "" {
		p.model = model
	}
	if p.created == 0 {
		p.created = created
	}
	res.Response = responseFromTurn(p, res)
}

func (r *turnRunner) emitError(err error) {
	r.updates <- toolproxy.TurnFinalResult{Err: err}
	r.mu.Lock()
	chat := r.chat.take()
	response := r.response.take()
	r.mu.Unlock()
	if chat.send(StreamEvent{Kind: "error", Error: err}) {
		chat.close()
	}
	if response.send(ResponseStreamEvent{Kind: "error", Error: err}) {
		response.close()
	}
}

// failSend surfaces an async session.Send failure through the runner loop as a
// synthetic SessionError event, rather than emitting from the Send goroutine.
// Routing it through the loop keeps emitError/emitResult/closeStreams
// single-owner (loop-only), so an async send failure cannot race the loop's
// concurrent stream sends and channel closes. The sink keeps the delivery
// ordered behind any buffered events and never blocks this goroutine.
func (r *turnRunner) failSend(events *sessionEventSink, err error) {
	events.send(copilot.SessionEvent{Data: &copilot.SessionErrorData{Message: err.Error()}})
}

func (r *turnRunner) addPin(release func()) {
	if release == nil {
		return
	}
	r.pinMu.Lock()
	if r.pinsReleased {
		r.pinMu.Unlock()
		release()
		return
	}
	r.pinReleases = append(r.pinReleases, release)
	r.pinMu.Unlock()
}

func (r *turnRunner) releasePins() {
	r.pinMu.Lock()
	if r.pinsReleased {
		r.pinMu.Unlock()
		return
	}
	r.pinsReleased = true
	releases := r.pinReleases
	r.pinReleases = nil
	r.pinMu.Unlock()
	for _, release := range releases {
		release()
	}
}

func (r *turnRunner) closeStreams() {
	r.mu.Lock()
	chat := r.chat.take()
	response := r.response.take()
	r.mu.Unlock()
	chat.close()
	response.close()
}

func (r *turnRunner) result(text, reasoning string, usage *openai.Usage, finish string) *TurnResult {
	r.mu.Lock()
	id := r.id
	r.mu.Unlock()
	return &TurnResult{ID: id, Created: r.created, Model: r.model, SDKSessionID: r.sessionID(), Text: text, Reasoning: reasoning, Usage: usage, FinishReason: finish, RetainedPath: r.retained}
}

// usageFromSDK maps an SDK usage event onto the Chat usage object. The mapping
// is all-or-nothing by contract: OpenAI declares the three counters required, so
// a usage event that reports no token counts produces no usage object at all
// rather than one carrying whichever fields happened to arrive. The SDK reports
// input and output tokens independently, so the partial case is reachable.
func usageFromSDK(d *copilot.AssistantUsageData) *openai.Usage {
	if d == nil || (d.InputTokens == nil && d.OutputTokens == nil) {
		return nil
	}
	usage := &openai.Usage{}
	if d.InputTokens != nil {
		usage.PromptTokens = *d.InputTokens
	}
	if d.OutputTokens != nil {
		usage.CompletionTokens = *d.OutputTokens
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	if d.ReasoningTokens != nil {
		v := *d.ReasoningTokens
		usage.CompletionTokensDetails = &openai.TokenDetails{ReasoningTokens: &v}
	}
	// Prompt-cache accounting. The SDK reports both halves and this proxy used
	// to drop them, which mattered once the counters stopped being optional:
	// input_tokens_details is now always on the wire, so omitting the source
	// turned "unknown" into a positive claim of zero cache reuse on every turn.
	// Clients act on it - Codex maps cached_tokens straight into its cost
	// display - so a hardcoded 0 is a wrong number, not a missing one.
	if d.CacheReadTokens != nil || d.CacheWriteTokens != nil {
		usage.PromptTokensDetails = &openai.PromptTokenDetails{CachedTokens: d.CacheReadTokens, CacheWriteTokens: d.CacheWriteTokens}
	}
	return usage
}

func chatToolCallsFromCaptured(calls []toolproxy.CapturedCall) []openai.ChatToolCall {
	out := make([]openai.ChatToolCall, 0, len(calls))
	for _, call := range calls {
		name := call.ResponseName
		if name == "" {
			name = call.SDKName
		}
		out = append(out, openai.ChatToolCall{ID: call.CallID, Type: "function", Function: openai.ToolCallFunction{Name: name, Arguments: string(call.ArgumentsJSON)}})
	}
	return out
}

// responseFromTurn builds the openai.Response for a turn result. It is the one
// and only place a Responses payload is constructed from a turn: the streamed
// terminal event, the persisted record and the non-streaming JSON body all
// serialize the value it returns. Callers on the live path reach it through
// turnRunner.buildTurnResponse, which caches the result on the turn so a second
// construction cannot happen.
//
// Every identity decision is made here or upstream of here: the output-item IDs
// come from the turn (assigned by the runner before the first streamed delta
// carried them to the client), the creation timestamp is the runner's, and the
// output order is the order the stream announced the items in.
func responseFromTurn(p responseParams, turn *TurnResult) *openai.Response {
	turn.responseBuilds++
	id := p.id
	if id == "" {
		id = openai.NewID("resp_")
	}
	// One response, one creation time: the request stamps it, the runner's own
	// clock is the fallback for a turn built without one, and only a turn built
	// outside both (tests, synthetic results) reads the clock here.
	created := p.created
	if created == 0 {
		created = turn.Created
	}
	if created == 0 {
		created = openai.UnixNow()
	}
	resp := &openai.Response{ID: id, Object: openai.ObjectResponse, CreatedAt: created, Status: "completed", Model: p.model, Instructions: p.instructions, Output: []openai.ResponseOutputItem{}, OutputText: turn.Text, ParallelToolCalls: true, PreviousResponseID: p.previous, Store: p.store, Metadata: p.metadata, Usage: openai.NewResponseUsage(turn.Usage), Error: nil, IncompleteDetails: nil}
	// The response is always built complete. Reasoning-emission policy is a
	// presentation concern applied at the edge (internal/httpapi), never here:
	// this object is also the persisted record, so filtering it would make an
	// operator's current display preference permanently destroy stored data.
	if item, ok := reasoningOutputItem(turn); ok {
		resp.Output = append(resp.Output, item)
	}
	calls := turn.ResponseToolCalls
	if len(calls) == 0 && len(turn.ToolCalls) > 0 {
		calls = capturedFromChatToolCalls(turn.ToolCalls)
	}
	// A message item the stream already announced must exist in the terminal
	// output even if the turn ended with empty text, or the client would be left
	// holding an item that never lands in the stored record.
	if turn.Text != "" || len(calls) == 0 || turn.streamedMessage() {
		resp.Output = append(resp.Output, openai.ResponseOutputItem{ID: turn.messageOutputItemID(), Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: turn.Text}}})
	}
	for _, tc := range calls {
		resp.Output = append(resp.Output, responseOutputItemFromCaptured(tc))
	}
	resp.Output = orderOutputItems(resp.Output, turn.itemOrder)
	return resp
}

// responseForTurn returns the turn's single response. The runner builds it for
// every turn whose response identity is known, so this only constructs one for
// a continuation whose runner was already forgotten - never a second time for
// the same turn.
func responseForTurn(p responseParams, turn *TurnResult) *openai.Response {
	if turn.Response != nil {
		return turn.Response
	}
	turn.Response = responseFromTurn(p, turn)
	return turn.Response
}

// warmResponseCreatedAt uses the creation time the request was stamped with, so
// a warm response reports the same instant on the wire and in the store.
func warmResponseCreatedAt(req ResponseRequest) int64 {
	if req.CreatedAt != 0 {
		return req.CreatedAt
	}
	return openai.UnixNow()
}

// orderOutputItems arranges output items in the order the response stream
// announced them, keeping items that were never streamed in their natural
// order behind the announced ones. This is what makes a streamed event's
// output_index equal to the item's position in the stored response, instead of
// relying on the incidental fact that reasoning happens to be built first.
func orderOutputItems(items []openai.ResponseOutputItem, order []string) []openai.ResponseOutputItem {
	if len(order) == 0 || len(items) < 2 {
		return items
	}
	out := make([]openai.ResponseOutputItem, 0, len(items))
	taken := make([]bool, len(items))
	for _, id := range order {
		for i, item := range items {
			if !taken[i] && item.ID != "" && item.ID == id {
				out = append(out, item)
				taken[i] = true
				break
			}
		}
	}
	for i, item := range items {
		if !taken[i] {
			out = append(out, item)
		}
	}
	return out
}

func capturedFromChatToolCalls(calls []openai.ChatToolCall) []toolproxy.CapturedCall {
	out := make([]toolproxy.CapturedCall, 0, len(calls))
	for _, tc := range calls {
		out = append(out, toolproxy.CapturedCall{Kind: toolcatalog.ToolKindFunction, ResponseName: tc.Function.Name, CallID: tc.ID, ArgumentsJSON: jsonRaw(tc.Function.Arguments)})
	}
	return out
}

// responseToolItemID is the Responses output-item id for a tool call of the
// given kind. The streaming path and the terminal response both derive an
// item's id from here, so a call's fragments and its finished item can never
// name two different items.
func responseToolItemID(kind toolcatalog.ResponsesToolKind, callID string) string {
	switch kind {
	case toolcatalog.ToolKindCustom:
		return "ctc_" + callID
	case toolcatalog.ToolKindToolSearch:
		return "tsc_" + callID
	default:
		return "fc_" + callID
	}
}

func responseOutputItemFromCaptured(tc toolproxy.CapturedCall) openai.ResponseOutputItem {
	kind := tc.Kind
	if kind == "" {
		kind = toolcatalog.ToolKindFunction
	}
	name := tc.ResponseName
	if name == "" {
		name = tc.SDKName
	}
	switch kind {
	case toolcatalog.ToolKindCustom:
		return openai.ResponseOutputItem{ID: responseToolItemID(kind, tc.CallID), Type: "custom_tool_call", Status: "completed", CallID: tc.CallID, Name: name, Input: tc.Input}
	case toolcatalog.ToolKindToolSearch:
		execution := tc.Execution
		if execution == "" {
			execution = "client"
		}
		args := tc.ArgumentsJSON
		if len(args) == 0 {
			args = jsonRaw(`{}`)
		}
		return openai.ResponseOutputItem{ID: responseToolItemID(kind, tc.CallID), Type: "tool_search_call", Status: "completed", CallID: tc.CallID, Execution: execution, ArgumentsJSON: args}
	default:
		return openai.ResponseOutputItem{ID: responseToolItemID(kind, tc.CallID), Type: "function_call", Status: "completed", CallID: tc.CallID, Namespace: tc.Namespace, Name: name, Arguments: string(tc.ArgumentsJSON)}
	}
}

func jsonRaw(s string) json.RawMessage { return json.RawMessage(s) }

// messageOutputItemID returns the turn's message output-item ID, assigning one
// on first use. The runner normally assigns it before the first streamed delta;
// memoizing here keeps a turn built outside a runner stable too.
func (t *TurnResult) messageOutputItemID() string {
	if t.MessageItemID == "" {
		t.MessageItemID = openai.NewID("msg_")
	}
	return t.MessageItemID
}

// reasoningOutputItemID returns the turn's reasoning output-item ID, deriving a
// stable ID from the SDK reasoning block ID when one is available so streamed
// and final items agree.
func (t *TurnResult) reasoningOutputItemID() string {
	if t.ReasoningItemID == "" {
		if t.ReasoningID != "" {
			t.ReasoningItemID = "rs_" + t.ReasoningID
		} else {
			t.ReasoningItemID = openai.NewID("rs_")
		}
	}
	return t.ReasoningItemID
}

// streamedMessage reports whether this turn already announced its message item
// on the response stream.
func (t *TurnResult) streamedMessage() bool {
	if t.MessageItemID == "" {
		return false
	}
	for _, id := range t.itemOrder {
		if id == t.MessageItemID {
			return true
		}
	}
	return false
}

// reasoningOutputItem builds the Responses `reasoning` output item from a turn,
// carrying the plaintext summary plus any OpenAI-style encrypted continuation
// blob. It reports false when the turn produced no reasoning.
func reasoningOutputItem(turn *TurnResult) (openai.ResponseOutputItem, bool) {
	if turn.Reasoning == "" && turn.ReasoningEncrypted == "" {
		return openai.ResponseOutputItem{}, false
	}
	item := openai.ResponseOutputItem{ID: turn.reasoningOutputItemID(), Type: "reasoning", Status: "completed", EncryptedContent: turn.ReasoningEncrypted}
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
	return sessionstore.ResponseRecord{ID: resp.ID, SDKSessionID: sessionID, Model: resp.Model, Instructions: resp.Instructions, CreatedAt: time.Unix(resp.CreatedAt, 0).UTC(), UpdatedAt: time.Now().UTC(), Status: resp.Status, Stored: resp.Store, Output: resp.Output, OutputText: resp.OutputText, Usage: resp.Usage, Metadata: resp.Metadata, PreviousResponseID: previous, RetainedPath: retained}
}
