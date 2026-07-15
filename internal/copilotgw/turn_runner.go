package copilotgw

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/observability"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolproxy"

	copilot "github.com/github/copilot-sdk/go"
)

type turnRunner struct {
	id             string
	model          string
	ctx            context.Context
	session        *copilot.Session
	rt             *toolproxy.RequestTools
	events         <-chan copilot.SessionEvent
	retained       string
	kind           string
	maxOutputBytes int64

	responseID string
	created    int64
	batch      *toolproxy.Batch
	updates    chan toolproxy.TurnFinalResult
	closed     chan struct{}

	chatStream        chan<- StreamEvent
	chatDone          <-chan struct{}
	mu                sync.Mutex
	abortOnce         sync.Once
	requestDetached   bool
	requestGeneration uint64
	responseStream    chan<- ResponseStreamEvent
	responseMeta      *responseStreamMeta
	onResult          func(*TurnResult) error
	store             *sessionstore.Store
	pinMu             sync.Mutex
	pinReleases       []func()
	pinsReleased      bool
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
	maxOutputBytes := g.cfg.MaxTurnOutputBytes
	if maxOutputBytes <= 0 {
		maxOutputBytes = config.DefaultMaxTurnOutputBytes
	}
	r := &turnRunner{id: id, model: model, ctx: ctx, session: session, rt: rt, events: events, retained: retained, kind: kind, maxOutputBytes: maxOutputBytes, responseID: responseID, updates: make(chan toolproxy.TurnFinalResult, 16), closed: make(chan struct{}), created: openai.UnixNow(), store: g.store}
	if g.store != nil && session != nil {
		r.addPin(g.store.PinSession(session.SessionID))
	}
	if kind == "response" && responseID != "" && g.store != nil {
		r.addPin(g.store.PinResponse(responseID))
	}
	if g.active == nil {
		g.active = newActiveRunnerRegistry()
	}
	if !g.active.add(r) {
		r.abort()
		r.releasePins()
		close(r.closed)
		return r
	}
	go r.loop(g)
	return r
}

func (r *turnRunner) discardInitial() {
	<-r.updates
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

// currentResponseID follows active continuation metadata so a reused runner
// parks tool calls under the continuation response id, not the id from the
// original request that created the SDK session.
func (r *turnRunner) currentResponseID() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.responseMeta != nil && r.responseMeta.responseID != "" {
		return r.responseMeta.responseID
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
			return nil, openai.Internal(fmt.Sprintf("unexpected turn result %T", first.Value))
		}
		return res, nil
	case <-ctx.Done():
		return nil, requestContextError(ctx)
	}
}

func requestContextError(ctx context.Context) error {
	if ctx != nil && ctx.Err() == context.DeadlineExceeded {
		return openai.Timeout()
	}
	return context.Canceled
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
	if g != nil && g.active != nil {
		defer g.active.remove(r)
	}
	defer r.closeStreams()
	defer r.releasePins()
	var text string
	var reason reasoningAccumulator
	var usage *openai.Usage
	var contentBytes int64
	var reasoningStreamBytes int64
	stats := newTurnDebugStats()
	debugEnabled := g != nil && g.log != nil && g.log.Enabled(r.ctx, slog.LevelDebug)
	for event := range r.events {
		switch d := event.Data.(type) {
		case *copilot.AssistantTurnStartData:
			reason.reset()
			text = ""
			usage = nil
			contentBytes = 0
			reasoningStreamBytes = 0
			stats.reset()
			r.debug(g, "copilot turn started")
		case *copilot.AssistantMessageStartData:
			r.debug(g, "copilot assistant message started", "message_id", d.MessageID, "phase", optionalString(d.Phase), "ms_since_turn_start", stats.msSinceTurnStart())
		case *copilot.AssistantReasoningDeltaData:
			if contentBytes+reasoningStreamBytes+int64(len(d.DeltaContent)) > r.maxOutputBytes || contentBytes+reason.retainedSizeAfterDelta(d.DeltaContent) > r.maxOutputBytes {
				r.emitError(openai.Upstream("copilot reasoning output exceeded size limit"))
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
					r.emitError(openai.Upstream("copilot output exceeded size limit"))
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
		case *copilot.AssistantReasoningData:
			if contentBytes+reason.retainedSizeAfterConsolidated(d.Content) > r.maxOutputBytes {
				r.emitError(openai.Upstream("copilot reasoning output exceeded size limit"))
				r.abort()
				return
			}
			// Consolidated reasoning block; in tool-call turns this can arrive
			// after the message. If we already emitted that tool-call turn, do not
			// let its late final block seed the next continuation turn.
			reason.addConsolidated(d.Content, d.ReasoningID)
			r.debug(g, "copilot final reasoning block", "reasoning_id", d.ReasoningID, "content_bytes", len(d.Content), "content_runes", len([]rune(d.Content)), "ms_since_turn_start", stats.msSinceTurnStart())
		case *copilot.AssistantMessageData:
			toolRequestBytes, err := toolRequestPayloadSize(d.ToolRequests)
			if err != nil {
				r.emitError(openai.Upstream("failed to measure copilot tool-call output"))
				r.abort()
				return
			}
			reasoningText := reason.resolve()
			if d.ReasoningText != nil && *d.ReasoningText != "" {
				reasoningText = *d.ReasoningText
			}
			reasoningBytes := len(reasoningText) + optionalStringByteLen(d.ReasoningOpaque) + optionalStringByteLen(d.EncryptedContent)
			if int64(len(d.Content)+reasoningBytes)+toolRequestBytes > r.maxOutputBytes {
				r.emitError(openai.Upstream("copilot output exceeded size limit"))
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
			r.debug(g, "copilot final assistant message", append([]any{"message_id", d.MessageID, "content_bytes", len(d.Content), "content_runes", len([]rune(d.Content)), "reasoning_text_bytes", optionalStringByteLen(d.ReasoningText), "tool_request_count", len(d.ToolRequests)}, stats.summaryAttrs()...)...)
			if len(d.ToolRequests) > 0 {
				text = d.Content
				batch, calls, err := r.rt.CaptureRequests(d.ToolRequests, r.currentResponseID(), r.kind, r.model, r.updates, r.abort)
				if err != nil {
					r.emitError(openai.Upstream(err.Error()))
					r.abort()
					return
				}
				r.setBatch(batch)
				res := r.result(text, reason.resolve(), usage, "tool_calls")
				reason.applyTo(res)
				res.ResponseToolCalls = calls
				res.ToolCalls = chatToolCallsFromCaptured(calls)
				res.PendingBatchID = batch.ID
				r.emitResult(res)
				// The runner loop is reused across the client-owned tool-call
				// continuation, so each tool turn must start a fresh reasoning
				// block. Without this reset the next turn would inherit (or
				// concatenate) this turn's reasoning when its own consolidated
				// block is absent.
				text = ""
				usage = nil
				contentBytes = 0
				reasoningStreamBytes = 0
				reason.markToolBoundary()
				stats.reset()
			} else {
				text = d.Content
			}
		case *copilot.AssistantStreamingDeltaData:
			stats.observeStreamProgress(d.TotalResponseSizeBytes)
			r.debug(g, "copilot stream progress", "total_response_size_bytes", d.TotalResponseSizeBytes, "stream_progress_count", stats.streamProgressCount, "ms_since_turn_start", stats.msSinceTurnStart())
		case *copilot.AssistantUsageData:
			usage = usageFromSDK(d)
			r.debug(g, "copilot usage received", "input_tokens", optionalInt(d.InputTokens), "output_tokens", optionalInt(d.OutputTokens), "reasoning_tokens", optionalInt(d.ReasoningTokens), "ms_since_turn_start", stats.msSinceTurnStart())
		case *copilot.SessionErrorData:
			err := openai.Upstream(d.Message)
			r.debug(g, "copilot session error", "error", d.Message, "ms_since_turn_start", stats.msSinceTurnStart())
			r.emitError(err)
			_ = r.session.Disconnect()
			return
		case *copilot.SessionIdleData:
			res := r.result(text, reason.resolve(), usage, "stop")
			reason.applyTo(res)
			r.debug(g, "copilot session idle", append([]any{"finish_reason", res.FinishReason, "final_text_bytes", len(res.Text), "final_text_runes", len([]rune(res.Text)), "final_reasoning_bytes", len(res.Reasoning)}, stats.summaryAttrs()...)...)
			r.emitResult(res)
			_ = r.session.Disconnect()
			return
		}
	}
	r.debug(g, "copilot session event stream ended before idle")
	r.emitError(openai.Upstream("copilot session event stream ended before idle"))
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
		"delta_runes", len([]rune(delta)),
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
	return r.session.SessionID
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
			return
		}
	}
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
	if res.FinishReason == "tool_calls" {
		// Once a tool-call result exists, its batch TTL owns liveness. Detach
		// before publishing to any transport so cancellation cannot win the narrow
		// interval between result delivery and detachment.
		r.detachFromRequestContext()
	}
	r.updates <- toolproxy.TurnFinalResult{Value: res}
	if chatStream != nil {
		sent := sendChatStreamEvent(chatStream, chatDone, StreamEvent{Kind: "result", Result: res})
		if res.FinishReason == "tool_calls" && sent {
			close(chatStream)
		}
	}
	if responseStream != nil {
		responseID := res.ID
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

// failSend surfaces an async session.Send failure through the runner loop as a
// synthetic SessionError event, rather than emitting from the Send goroutine.
// Routing it through the loop keeps emitError/emitResult/closeStreams
// single-owner (loop-only), so an async send failure cannot race the loop's
// concurrent stream sends and channel closes. The select on r.closed avoids
// blocking if the loop has already terminated (turn completed).
func (r *turnRunner) failSend(events chan<- copilot.SessionEvent, err error) {
	select {
	case events <- copilot.SessionEvent{Data: &copilot.SessionErrorData{Message: err.Error()}}:
	case <-r.closed:
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
	r.mu.Lock()
	id := r.id
	r.mu.Unlock()
	return &TurnResult{ID: id, Created: r.created, Model: r.model, SDKSessionID: r.session.SessionID, Text: text, Reasoning: reasoning, Usage: usage, FinishReason: finish, RetainedPath: r.retained}
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
	calls := turn.ResponseToolCalls
	if len(calls) == 0 && len(turn.ToolCalls) > 0 {
		calls = capturedFromChatToolCalls(turn.ToolCalls)
	}
	if turn.Text != "" || len(calls) == 0 {
		resp.Output = append(resp.Output, openai.ResponseOutputItem{ID: openai.NewID("msg_"), Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: turn.Text}}})
	}
	for _, tc := range calls {
		resp.Output = append(resp.Output, responseOutputItemFromCaptured(tc))
	}
	return resp
}

func capturedFromChatToolCalls(calls []openai.ChatToolCall) []toolproxy.CapturedCall {
	out := make([]toolproxy.CapturedCall, 0, len(calls))
	for _, tc := range calls {
		out = append(out, toolproxy.CapturedCall{Kind: openai.ToolKindFunction, ResponseName: tc.Function.Name, CallID: tc.ID, ArgumentsJSON: jsonRaw(tc.Function.Arguments)})
	}
	return out
}

func responseOutputItemFromCaptured(tc toolproxy.CapturedCall) openai.ResponseOutputItem {
	kind := tc.Kind
	if kind == "" {
		kind = openai.ToolKindFunction
	}
	name := tc.ResponseName
	if name == "" {
		name = tc.SDKName
	}
	switch kind {
	case openai.ToolKindCustom:
		return openai.ResponseOutputItem{ID: "ctc_" + tc.CallID, Type: "custom_tool_call", Status: "completed", CallID: tc.CallID, Name: name, Input: tc.Input}
	case openai.ToolKindToolSearch:
		execution := tc.Execution
		if execution == "" {
			execution = "client"
		}
		args := tc.ArgumentsJSON
		if len(args) == 0 {
			args = jsonRaw(`{}`)
		}
		return openai.ResponseOutputItem{ID: "tsc_" + tc.CallID, Type: "tool_search_call", Status: "completed", CallID: tc.CallID, Execution: execution, ArgumentsJSON: args}
	default:
		return openai.ResponseOutputItem{ID: "fc_" + tc.CallID, Type: "function_call", Status: "completed", CallID: tc.CallID, Namespace: tc.Namespace, Name: name, Arguments: string(tc.ArgumentsJSON)}
	}
}

func jsonRaw(s string) json.RawMessage { return json.RawMessage(s) }

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
	return sessionstore.ResponseRecord{ID: resp.ID, SDKSessionID: sessionID, Model: resp.Model, Instructions: resp.Instructions, CreatedAt: time.Unix(resp.CreatedAt, 0).UTC(), UpdatedAt: time.Now().UTC(), Status: resp.Status, Stored: resp.Store, Output: storeOutputItems(resp.Output), OutputText: resp.OutputText, Usage: storeUsage(resp.Usage), PreviousResponseID: previous, RetainedPath: retained}
}
