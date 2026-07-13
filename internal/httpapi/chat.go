package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req openai.ChatCompletionRequest
	if err := decodeJSON(w, r, s.cfg.MaxRequestBodyBytes, &req); err != nil {
		openai.WriteError(w, err)
		return
	}
	if err := openai.ValidateChatRequest(&req, s.cfg.StrictCompat); err != nil {
		openai.WriteError(w, err)
		return
	}
	instructions, messages, err := openai.FoldChatInstructions(req.Messages)
	if err != nil {
		openai.WriteError(w, err)
		return
	}
	ctx, cancel := requestContext(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	if isToolContinuation(messages) {
		s.logGenerationStarted(r, "chat.completions", req.Model, req.ReasoningEffort, "", false, true)
		outputs, err := trailingToolOutputs(messages)
		if err != nil {
			openai.WriteError(w, err)
			return
		}
		contReq := copilotgw.ChatContinuationRequest{Model: req.Model, Instructions: instructions, Messages: messages, Outputs: outputs, Tools: req.Tools, ToolChoiceNone: openai.ToolChoiceNone(req.ToolChoice), ReasoningEffort: req.ReasoningEffort, DefaultReasoningEffort: s.cfg.DefaultReasoningEffort, IncludeUsageChunk: req.StreamOptions != nil && req.StreamOptions.IncludeUsage}
		if req.Stream {
			s.streamChatContinuation(w, r, contReq)
			return
		}
		turn, err := s.gw.ContinueChatToolCalls(ctx, contReq)
		if err != nil {
			openai.WriteError(w, err)
			return
		}
		turn.ID = openai.NewID("chatcmpl_")
		turn.Created = openai.UnixNow()
		writeJSON(w, http.StatusOK, s.chatCompletionFromTurn(turn))
		return
	}
	if len(messages) == 0 {
		openai.WriteError(w, openai.InvalidRequest("messages is required", "messages"))
		return
	}
	last := messages[len(messages)-1]
	if last.Role != "user" && last.Role != "assistant" {
		openai.WriteError(w, openai.InvalidRequest("Chat Completions requests must end with a user message, assistant prefill, or tool continuation", "messages"))
		return
	}
	if last.Role == "assistant" && len(last.ToolCalls) > 0 {
		openai.WriteError(w, openai.InvalidRequest("assistant tool calls require following tool messages", "messages"))
		return
	}
	resolvedEffort, resolved, err := s.resolveGenerationReasoningEffort(ctx, req.Model, req.ReasoningEffort)
	if err != nil {
		openai.WriteError(w, err)
		return
	}
	s.logGenerationStarted(r, "chat.completions", req.Model, req.ReasoningEffort, resolvedEffort, resolved, false)
	history := messages[:len(messages)-1]
	finalUser := messages[len(messages)-1]
	if last.Role == "assistant" {
		history = messages
		finalUser = openai.ChatMessage{Role: "user", Content: openai.NewTextContent("Continue.")}
	}
	chatReq := copilotgw.ChatRequest{OpenAIID: openai.NewID("chatcmpl_"), Model: req.Model, Instructions: instructions, History: history, FinalUser: finalUser, Tools: req.Tools, ToolChoiceNone: openai.ToolChoiceNone(req.ToolChoice), ReasoningEffort: req.ReasoningEffort, DefaultReasoningEffort: s.cfg.DefaultReasoningEffort, ResolvedReasoningEffort: resolvedEffort, ReasoningEffortResolved: resolved, IncludeUsageChunk: req.StreamOptions != nil && req.StreamOptions.IncludeUsage}
	if req.Stream {
		s.streamChat(w, r, chatReq)
		return
	}
	turn, err := s.gw.Chat(ctx, chatReq)
	if err != nil {
		openai.WriteError(w, err)
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
	writer, ok := openai.NewSSEWriter(w)
	if !ok {
		openai.WriteError(w, openai.Internal("streaming unsupported by ResponseWriter"))
		return
	}
	ctx, cancel := requestContext(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	ch, err := start(ctx)
	if err != nil {
		_ = writer.Data(openai.ErrorEnvelope{Error: errorObject(err)})
		_ = writer.Done()
		return
	}
	created := openai.UnixNow()
	if err := s.writeSSEData(ctx, writer, "chat.role", openai.ChatCompletionChunk{ID: streamID, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Role: "assistant"}}}, IncludeUsage: includeUsage}, "stream_kind", "chat", "chunk_kind", "role"); err != nil {
		return
	}
	for ev := range ch {
		s.logChatStreamEvent(ctx, ev)
		switch ev.Kind {
		case "reasoning_delta":
			if err := s.writeChatReasoningDelta(ctx, writer, streamID, created, model, ev.Delta, includeUsage); err != nil {
				return
			}
		case "delta":
			if err := s.writeSSEData(ctx, writer, "chat.content_delta", openai.ChatCompletionChunk{ID: streamID, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Content: ev.Delta}}}, IncludeUsage: includeUsage}, s.chatChunkAttrs(ctx, "content", ev.Delta)...); err != nil {
				return
			}
		case "result":
			if err := s.writeChatTerminalWithID(ctx, writer, streamID, created, model, ev.Result, includeUsage); err != nil {
				return
			}
		case "error":
			if err := s.writeSSEData(ctx, writer, "chat.error", openai.ErrorEnvelope{Error: errorObject(ev.Error)}, "stream_kind", "chat", "chunk_kind", "error"); err != nil {
				return
			}
		}
	}
	s.debugStream(ctx, "chat stream channel closed", "stream_kind", "chat", "stream_id", streamID)
	_ = s.writeSSEDone(ctx, writer, "stream_kind", "chat")
}
func (s *Server) writeChatReasoningDelta(ctx context.Context, writer *openai.SSEWriter, id string, created int64, model, delta string, includeUsage bool) error {
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
func (s *Server) writeChatTerminalWithID(ctx context.Context, writer *openai.SSEWriter, id string, created int64, model string, turn *copilotgw.TurnResult, includeUsage bool) error {
	finish := turn.FinishReason
	if details := s.chatReasoningDetails(turn); len(details) > 0 {
		// The plaintext reasoning was already streamed as deltas; this terminal
		// chunk carries the structured details (signature + encrypted blob) so
		// clients can replay reasoning for continuity.
		if err := s.writeSSEData(ctx, writer, "chat.reasoning_details", openai.ChatCompletionChunk{ID: id, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{ReasoningDetails: details}}}, IncludeUsage: includeUsage}, "stream_kind", "chat", "chunk_kind", "reasoning_details", "reasoning_detail_count", len(details)); err != nil {
			return err
		}
	}
	if len(turn.ToolCalls) > 0 {
		deltas := make([]openai.ToolCallDelta, 0, len(turn.ToolCalls))
		for i, tc := range turn.ToolCalls {
			deltas = append(deltas, openai.ToolCallDelta{Index: i, ID: tc.ID, Type: "function", Function: &openai.ToolCallDeltaFunction{Name: tc.Function.Name, Arguments: tc.Function.Arguments}})
		}
		if err := s.writeSSEData(ctx, writer, "chat.tool_calls", openai.ChatCompletionChunk{ID: id, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{ToolCalls: deltas}}}, IncludeUsage: includeUsage}, "stream_kind", "chat", "chunk_kind", "tool_calls", "tool_call_count", len(turn.ToolCalls)); err != nil {
			return err
		}
	}
	if err := s.writeSSEData(ctx, writer, "chat.finish", openai.ChatCompletionChunk{ID: id, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{}, FinishReason: &finish}}, IncludeUsage: includeUsage}, "stream_kind", "chat", "chunk_kind", "finish", "finish_reason", finish); err != nil {
		return err
	}
	if includeUsage {
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
func trailingToolOutputs(messages []openai.ChatMessage) (map[string]string, error) {
	outputs := map[string]string{}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "tool" {
			break
		}
		if _, dup := outputs[messages[i].ToolCallID]; dup {
			return nil, openai.InvalidRequest("duplicate tool_call_id in tool outputs", fmt.Sprintf("messages.%d.tool_call_id", i))
		}
		out, err := toolOutputFromContent(messages[i].Content)
		if err != nil {
			return nil, openai.InvalidRequest(err.Error(), fmt.Sprintf("messages.%d.content", i))
		}
		outputs[messages[i].ToolCallID] = out
	}
	return outputs, nil
}
func toolOutputFromContent(content openai.Content) (string, error) {
	if !content.Present || content.IsNull {
		return "", nil
	}
	if s, err := content.Text(); err == nil {
		return s, nil
	}
	var v any
	if err := json.Unmarshal(content.Raw, &v); err != nil {
		return "", err
	}
	switch v.(type) {
	case map[string]any, []any:
		b, _ := json.Marshal(v)
		return string(b), nil
	default:
		return "", fmt.Errorf("tool output must be string, text parts, JSON object, or JSON array")
	}
}
