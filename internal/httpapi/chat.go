package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req openai.ChatCompletionRequest
	if err := decodeJSON(w, r, s.cfg.MaxRequestBodyBytes, &req); err != nil {
		WriteError(w, err)
		return
	}
	selector, err := openai.ParseModelSelector(req.Model)
	if err != nil {
		WriteError(w, err)
		return
	}
	reasoningEffort, err := openai.MergeReasoningEffort(selector, req.ReasoningEffort, "reasoning_effort")
	if err != nil {
		WriteError(w, err)
		return
	}
	req.Model = selector.Model
	req.ReasoningEffort = reasoningEffort
	if err := openai.ValidateChatRequest(&req); err != nil {
		WriteError(w, err)
		return
	}
	instructions, messages, err := openai.FoldChatInstructions(req.Messages)
	if err != nil {
		WriteError(w, err)
		return
	}
	ctx, cancel := requestContext(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	s.logUnhonoredChatControls(ctx, &req)
	toolChoice, err := openai.ParseToolChoice(req.ToolChoice)
	if err != nil {
		WriteError(w, err)
		return
	}
	if isToolContinuation(messages) {
		s.logGenerationStarted(r, "chat.completions", req.Model, req.ReasoningEffort, "", false, true)
		outputs, err := trailingToolOutputs(messages)
		if err != nil {
			WriteError(w, err)
			return
		}
		contReq := copilotgw.ChatContinuationRequest{Model: req.Model, Instructions: instructions, Messages: messages, Outputs: outputs, Tools: req.Tools, ToolChoice: toolChoice, ReasoningEffort: req.ReasoningEffort, DefaultReasoningEffort: s.cfg.DefaultReasoningEffort, IncludeUsageChunk: req.StreamOptions != nil && req.StreamOptions.IncludeUsage}
		if req.Stream {
			s.streamChatContinuation(w, r, contReq)
			return
		}
		turn, err := s.gw.ContinueChatToolCalls(ctx, contReq)
		if err != nil {
			WriteError(w, err)
			return
		}
		turn.ID = openai.NewID("chatcmpl_")
		turn.Created = openai.UnixNow()
		writeJSON(w, http.StatusOK, s.chatCompletionFromTurn(turn))
		return
	}
	if len(messages) == 0 {
		WriteError(w, apierr.InvalidRequest("messages is required", "messages"))
		return
	}
	last := messages[len(messages)-1]
	if last.Role != "user" && last.Role != "assistant" {
		WriteError(w, apierr.InvalidRequest("Chat Completions requests must end with a user message, assistant prefill, or tool continuation", "messages"))
		return
	}
	if last.Role == "assistant" && len(last.ToolCalls) > 0 {
		WriteError(w, apierr.InvalidRequest("assistant tool calls require following tool messages", "messages"))
		return
	}
	resolvedEffort, resolved, err := s.resolveGenerationReasoningEffort(ctx, req.Model, req.ReasoningEffort)
	if err != nil {
		WriteError(w, err)
		return
	}
	s.logGenerationStarted(r, "chat.completions", req.Model, req.ReasoningEffort, resolvedEffort, resolved, false)
	history := messages[:len(messages)-1]
	finalUser := messages[len(messages)-1]
	if last.Role == "assistant" {
		history = messages
		finalUser = openai.ChatMessage{Role: "user", Content: openai.NewTextContent("Continue.")}
	}
	chatReq := copilotgw.ChatRequest{OpenAIID: openai.NewID("chatcmpl_"), Model: req.Model, Instructions: instructions, History: history, FinalUser: finalUser, Tools: req.Tools, ToolChoice: toolChoice, ReasoningEffort: req.ReasoningEffort, DefaultReasoningEffort: s.cfg.DefaultReasoningEffort, ResolvedReasoningEffort: resolvedEffort, ReasoningEffortResolved: resolved, IncludeUsageChunk: req.StreamOptions != nil && req.StreamOptions.IncludeUsage}
	if req.Stream {
		s.streamChat(w, r, chatReq)
		return
	}
	turn, err := s.gw.Chat(ctx, chatReq)
	if err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.chatCompletionFromTurn(turn))
}
func (s *Server) streamChatContinuation(w http.ResponseWriter, r *http.Request, req copilotgw.ChatContinuationRequest) {
	s.streamChatEvents(w, r, openai.NewID("chatcmpl_"), req.Model, req.IncludeUsageChunk, func(ctx context.Context) (<-chan copilotgw.StreamEvent, error) {
		return s.gw.StreamContinueChatToolCalls(ctx, req)
	})
}

func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, req copilotgw.ChatRequest) {
	s.streamChatEvents(w, r, req.OpenAIID, req.Model, req.IncludeUsageChunk, func(ctx context.Context) (<-chan copilotgw.StreamEvent, error) {
		return s.gw.StreamChat(ctx, req)
	})
}

func (s *Server) streamChatEvents(w http.ResponseWriter, r *http.Request, streamID, model string, includeUsage bool, start func(context.Context) (<-chan copilotgw.StreamEvent, error)) {
	ctx, cancel := requestContext(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	// Complete synchronous gateway preflight before committing a 200 SSE
	// response, so validation/not-found/upstream setup errors retain their HTTP
	// status and standard JSON error envelope.
	ch, err := start(ctx)
	if err != nil {
		WriteError(w, err)
		return
	}
	writer, ok := NewSSEWriter(w)
	if !ok {
		WriteError(w, apierr.Internal("streaming unsupported by ResponseWriter"))
		return
	}
	// A turn can think for minutes before its first token, which any intermediary
	// reads as an idle connection. Stops on every exit path below.
	defer writer.KeepAlive(ctx, s.cfg.SSEKeepAliveInterval)()
	created := openai.UnixNow()
	// Headers are already committed, so every exit below this point is an HTTP
	// 200 no matter how the turn ends. Default to "abandoned" and let the
	// terminal paths overwrite it: the write-error returns scattered through the
	// loop are exactly the client-went-away case, and they have no other tell.
	defer markStreamAbandoned(r)
	writeFailure := func(streamErr error) {
		s.recordStreamOutcome(ctx, r, "chat.completions", streamOutcomeFailed, streamErr)
		if s.writeSSEData(ctx, writer, "chat.error", openai.ErrorEnvelope{Error: errorObject(streamErr)}, "stream_kind", "chat", "chunk_kind", "error") == nil {
			_ = s.writeSSEDone(ctx, writer, "stream_kind", "chat")
		}
	}
	// Once chunks are committed a panic can no longer be reported as an HTTP
	// error, so hand the recover middleware the chat stream's own grammar.
	setStreamFailureWriter(w, writeFailure)
	if err := s.writeSSEData(ctx, writer, "chat.role", openai.ChatCompletionChunk{ID: streamID, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Role: "assistant"}}}, IncludeUsage: includeUsage}, "stream_kind", "chat", "chunk_kind", "role"); err != nil {
		return
	}
	var streamedText strings.Builder
	var streamedReasoning strings.Builder
	var toolCalls chatToolCallStreams
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				writeFailure(apierr.Timeout())
			}
			return
		case ev, ok := <-ch:
			if !ok {
				if ctx.Err() == context.Canceled {
					return
				}
				if ctx.Err() == context.DeadlineExceeded {
					writeFailure(apierr.Timeout())
				} else {
					writeFailure(apierr.Upstream("chat stream ended before a terminal event"))
				}
				return
			}
			s.logChatStreamEvent(ctx, ev)
			switch ev.Kind {
			case "reasoning_delta":
				if err := s.writeChatReasoningDelta(ctx, writer, streamID, created, model, ev.Delta, includeUsage); err != nil {
					return
				}
				streamedReasoning.WriteString(ev.Delta)
			case "delta":
				if err := s.writeSSEData(ctx, writer, "chat.content_delta", openai.ChatCompletionChunk{ID: streamID, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Content: ev.Delta}}}, IncludeUsage: includeUsage}, s.chatChunkAttrs(ctx, "content", ev.Delta)...); err != nil {
					return
				}
				streamedText.WriteString(ev.Delta)
			case "tool_call_delta":
				if err := s.writeChatToolCallDelta(ctx, writer, streamID, created, model, &toolCalls, ev, includeUsage); err != nil {
					return
				}
			case "result":
				if ev.Result == nil {
					writeFailure(apierr.Internal("chat stream returned an empty result"))
					return
				}
				remainingReasoning, err := terminalStreamSuffix(ev.Result.Reasoning, streamedReasoning.String(), "chat stream terminal reasoning does not match streamed reasoning")
				if err != nil {
					writeFailure(err)
					return
				}
				if remainingReasoning != "" {
					if err := s.writeChatReasoningDelta(ctx, writer, streamID, created, model, remainingReasoning, includeUsage); err != nil {
						return
					}
				}
				remaining, err := terminalStreamSuffix(ev.Result.Text, streamedText.String(), "chat stream terminal text does not match streamed content")
				if err != nil {
					writeFailure(err)
					return
				}
				if remaining != "" {
					if err := s.writeSSEData(ctx, writer, "chat.content_delta", openai.ChatCompletionChunk{ID: streamID, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Content: remaining}}}, IncludeUsage: includeUsage}, s.chatChunkAttrs(ctx, "content", remaining)...); err != nil {
						return
					}
				}
				// Reconciled before any terminal chunk is written, because a
				// divergence between the streamed fragments and the finished call has
				// to be reported as a stream failure rather than as a write error.
				toolCallDeltas, err := toolCalls.terminalDeltas(ev.Result.ToolCalls)
				if err != nil {
					writeFailure(err)
					return
				}
				if err := s.writeChatTerminalWithID(ctx, writer, streamID, created, model, ev.Result, toolCallDeltas, includeUsage); err != nil {
					return
				}
				_ = s.writeSSEDone(ctx, writer, "stream_kind", "chat")
				s.recordStreamOutcome(ctx, r, "chat.completions", streamOutcomeCompleted, nil)
				return
			case "error":
				writeFailure(ev.Error)
				return
			}
		}
	}
}
func (s *Server) writeChatReasoningDelta(ctx context.Context, writer *SSEWriter, id string, created int64, model, delta string, includeUsage bool) error {
	if delta == "" {
		return nil
	}
	policy := openai.ResolveReasoningEmission(s.cfg.ReasoningEmission)
	if !policy.Enabled() {
		s.debugStream(ctx, "chat reasoning delta suppressed", s.chatChunkAttrs(ctx, "reasoning", delta)...)
		return nil
	}
	chunkDelta := openai.ChatChunkDelta{}
	if policy.EmitReasoning {
		chunkDelta.Reasoning = delta
	}
	if policy.EmitReasoningContent {
		chunkDelta.ReasoningContent = delta
	}
	return s.writeSSEData(ctx, writer, "chat.reasoning_delta", openai.ChatCompletionChunk{ID: id, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: chunkDelta}}, IncludeUsage: includeUsage}, s.chatChunkAttrs(ctx, "reasoning", delta)...)
}

// chatToolCallStreams remembers what a Chat Completions stream has already
// said about each tool call.
//
// OpenAI's wire shape for a streamed tool call is a stable `index` per call,
// with `id`, `type` and `name` carried once on that call's first fragment and
// nothing but `function.arguments` after it. Both halves of that need memory
// spanning chunks, and so does the terminal chunk: it may only send arguments
// the fragments have not already delivered.
type chatToolCallStreams struct {
	order    []string
	streamed map[string]string
}

// index assigns a call its wire index on first sight and reports whether this
// is that first sight, which is the same question as whether the identifying
// fields still have to be sent.
func (c *chatToolCallStreams) index(id string) (int, bool) {
	for i, seen := range c.order {
		if seen == id {
			return i, false
		}
	}
	c.order = append(c.order, id)
	return len(c.order) - 1, true
}

func (c *chatToolCallStreams) record(id, delta string) {
	if c.streamed == nil {
		c.streamed = map[string]string{}
	}
	c.streamed[id] += delta
}

// terminalDeltas reconciles a turn's finished tool calls against the fragments
// already delivered, returning only what the client is still owed.
//
// A call whose arguments were streamed in full contributes nothing: every
// client that reads this stream accumulates `function.arguments` across chunks,
// so repeating them here would double them. A call that was never streamed -
// a strict tool, or any turn on a backend that does not emit fragments -
// contributes the complete arguments exactly as it always has.
func (c *chatToolCallStreams) terminalDeltas(calls []openai.ChatToolCall) ([]openai.ToolCallDelta, error) {
	deltas := make([]openai.ToolCallDelta, 0, len(calls))
	delivered := make(map[string]struct{}, len(calls))
	for _, tc := range calls {
		delivered[tc.ID] = struct{}{}
		index, first := c.index(tc.ID)
		suffix, err := toolArgumentsSuffix(tc.Function.Arguments, c.streamed[tc.ID], "chat stream terminal tool-call arguments do not match the streamed arguments")
		if err != nil {
			return nil, err
		}
		if !first && suffix == "" {
			continue
		}
		delta := openai.ToolCallDelta{Index: index, Function: &openai.ToolCallDeltaFunction{Arguments: suffix}}
		if first {
			delta.ID = tc.ID
			delta.Type = "function"
			delta.Function.Name = tc.Function.Name
		}
		deltas = append(deltas, delta)
	}
	// A call the client accumulated fragments for and never receives is a call it
	// cannot answer, so it fails the stream rather than leaving the client to
	// discover the dangling index on its own.
	for id := range c.streamed {
		if _, ok := delivered[id]; !ok {
			return nil, apierr.Upstream("chat stream terminal result is missing tool call " + id + ", whose arguments were streamed")
		}
	}
	return deltas, nil
}

// writeChatToolCallDelta forwards one incremental tool-call argument fragment.
func (s *Server) writeChatToolCallDelta(ctx context.Context, writer *SSEWriter, id string, created int64, model string, streams *chatToolCallStreams, ev copilotgw.StreamEvent, includeUsage bool) error {
	if ev.ToolCallID == "" || ev.Delta == "" {
		return nil
	}
	index, first := streams.index(ev.ToolCallID)
	delta := openai.ToolCallDelta{Index: index, Function: &openai.ToolCallDeltaFunction{Arguments: ev.Delta}}
	if first {
		delta.ID = ev.ToolCallID
		delta.Type = "function"
		delta.Function.Name = ev.ToolName
	}
	streams.record(ev.ToolCallID, ev.Delta)
	return s.writeSSEData(ctx, writer, "chat.tool_call_delta", openai.ChatCompletionChunk{ID: id, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{ToolCalls: []openai.ToolCallDelta{delta}}}}, IncludeUsage: includeUsage}, append(s.chatChunkAttrs(ctx, "tool_call_delta", ev.Delta), "tool_call_id", ev.ToolCallID, "tool_call_index", index)...)
}

func (s *Server) writeChatTerminalWithID(ctx context.Context, writer *SSEWriter, id string, created int64, model string, turn *copilotgw.TurnResult, toolCallDeltas []openai.ToolCallDelta, includeUsage bool) error {
	finish := turn.FinishReason
	if details := s.chatReasoningDetails(turn); len(details) > 0 {
		// The plaintext reasoning was already streamed as deltas; this terminal
		// chunk carries the structured details (signature + encrypted blob) so
		// clients can replay reasoning for continuity.
		if err := s.writeSSEData(ctx, writer, "chat.reasoning_details", openai.ChatCompletionChunk{ID: id, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{ReasoningDetails: details}}}, IncludeUsage: includeUsage}, "stream_kind", "chat", "chunk_kind", "reasoning_details", "reasoning_detail_count", len(details)); err != nil {
			return err
		}
	}
	if len(toolCallDeltas) > 0 {
		if err := s.writeSSEData(ctx, writer, "chat.tool_calls", openai.ChatCompletionChunk{ID: id, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{ToolCalls: toolCallDeltas}}}, IncludeUsage: includeUsage}, "stream_kind", "chat", "chunk_kind", "tool_calls", "tool_call_count", len(toolCallDeltas)); err != nil {
			return err
		}
	}
	if err := s.writeSSEData(ctx, writer, "chat.finish", openai.ChatCompletionChunk{ID: id, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{}, FinishReason: &finish}}, IncludeUsage: includeUsage}, "stream_kind", "chat", "chunk_kind", "finish", "finish_reason", finish); err != nil {
		return err
	}
	// The terminal usage chunk exists only to carry usage: every other chunk
	// under stream_options.include_usage renders "usage": null by design, and
	// this is the one that is supposed to be populated. Emitting it with a null
	// payload would hand the client the exact null-arithmetic footgun that
	// making the counters required integers was meant to close, so when the
	// upstream reported no usage at all this chunk is omitted instead. The
	// preceding finish chunk already carried finish_reason, and [DONE] still
	// terminates the stream.
	if includeUsage && turn.Usage != nil {
		if err := s.writeSSEData(ctx, writer, "chat.usage", openai.ChatCompletionChunk{ID: id, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{}, Usage: turn.Usage, IncludeUsage: true}, "stream_kind", "chat", "chunk_kind", "usage"); err != nil {
			return err
		}
	}
	return nil
}
func (s *Server) chatCompletionFromTurn(turn *copilotgw.TurnResult) openai.ChatCompletion {
	msg := openai.ChatMessage{Role: "assistant", Content: openai.NewTextContent(turn.Text), ToolCalls: turn.ToolCalls}
	if turn.Text == "" && len(turn.ToolCalls) > 0 {
		msg.Content = openai.Content{Present: true, IsNull: true}
	}
	policy := openai.ResolveReasoningEmission(s.cfg.ReasoningEmission)
	if policy.Enabled() {
		if turn.Reasoning != "" {
			if policy.EmitReasoning {
				msg.Reasoning = turn.Reasoning
			}
			if policy.EmitReasoningContent {
				msg.ReasoningContent = turn.Reasoning
			}
		}
		if details := s.chatReasoningDetails(turn); len(details) > 0 {
			msg.ReasoningDetails = details
		}
	}
	return openai.ChatCompletion{ID: turn.ID, Object: openai.ObjectChatCompletion, Created: turn.Created, Model: turn.Model, Choices: []openai.ChatCompletionChoice{{Index: 0, Message: msg, FinishReason: turn.FinishReason}}, Usage: turn.Usage, SystemFingerprint: nil}
}

// chatReasoningDetails builds the structured reasoning_details for a turn,
// honoring the emission policy (an "off" policy suppresses them entirely).
func (s *Server) chatReasoningDetails(turn *copilotgw.TurnResult) []openai.ReasoningDetail {
	if !openai.ResolveReasoningEmission(s.cfg.ReasoningEmission).Enabled() {
		return nil
	}
	return openai.BuildReasoningDetails(turn.Reasoning, turn.ReasoningOpaque, turn.ReasoningEncrypted, turn.ReasoningID)
}
func isToolContinuation(messages []openai.ChatMessage) bool {
	return len(messages) > 0 && messages[len(messages)-1].Role == "tool"
}

// logUnhonoredChatControls reports the request controls this proxy accepts but
// cannot forward to the Copilot SDK. They are advisory rather than fatal because
// every mainstream OpenAI client sets at least one of them, but a debug line
// keeps the gap visible when a response does not match a client's expectation.
func (s *Server) logUnhonoredChatControls(ctx context.Context, req *openai.ChatCompletionRequest) {
	if !s.debugEnabled(ctx) {
		return
	}
	if tokens, ok := req.RequestedMaxOutputTokens(); ok {
		s.debugStream(ctx, "max output tokens is not forwarded to the Copilot SDK", "surface", "chat.completions", "max_output_tokens", tokens)
	}
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		s.debugStream(ctx, "parallel_tool_calls=false is not enforced by the Copilot SDK", "surface", "chat.completions")
	}
	if req.RequestedLogprobs() {
		s.debugStream(ctx, "logprobs are not produced by the Copilot SDK", "surface", "chat.completions")
	}
	if fields := openai.UnhonoredChatFields(req.Raw); len(fields) > 0 {
		s.debugStream(ctx, "request fields were accepted but are not acted on", "surface", "chat.completions", "fields", fields)
	}
	s.logUnhonoredToolChoice(ctx, "chat.completions", req.ToolChoice)
}

// logUnhonoredToolChoice reports the part of a tool_choice this proxy does not
// deliver. Narrowing the tool catalog is the only lever the Copilot SDK offers,
// so it says what narrowing did and does not claim more than that: an
// allow-list is satisfied exactly and goes unmentioned, while a demand that the
// model actually call something survives no matter how narrow the catalog gets.
func (s *Server) logUnhonoredToolChoice(ctx context.Context, surface string, raw json.RawMessage) {
	choice, err := openai.ParseToolChoice(raw)
	if err != nil || choice.Honored() {
		return
	}
	attrs := []any{"surface", surface, "tool_choice", choice.Kind}
	if choice.Name != "" {
		attrs = append(attrs, "tool_choice_name", choice.Name)
	}
	switch {
	case choice.ForcesTool():
		s.debugStream(ctx, "tool_choice narrows the tool catalog to the named tool, but the Copilot SDK cannot make the model call it", attrs...)
	case choice.Kind == "allowed_tools":
		s.debugStream(ctx, "allowed_tools narrows the tool catalog, but its required mode is not enforced by the Copilot SDK", attrs...)
	default:
		s.debugStream(ctx, "tool_choice is not enforced by the Copilot SDK", attrs...)
	}
}

func trailingToolOutputs(messages []openai.ChatMessage) (map[string]string, error) {
	outputs := map[string]string{}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "tool" {
			break
		}
		if _, dup := outputs[messages[i].ToolCallID]; dup {
			return nil, apierr.InvalidRequest("duplicate tool_call_id in tool outputs", fmt.Sprintf("messages.%d.tool_call_id", i))
		}
		out, err := messages[i].Content.ToolOutput()
		if err != nil {
			return nil, apierr.InvalidRequest(err.Error(), fmt.Sprintf("messages.%d.content", i))
		}
		outputs[messages[i].ToolCallID] = out
	}
	return outputs, nil
}
