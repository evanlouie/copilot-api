package httpapi

import (
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
		writeJSON(w, http.StatusOK, chatCompletionFromTurn(turn))
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
	writeJSON(w, http.StatusOK, chatCompletionFromTurn(turn))
}
func (s *Server) streamChatContinuation(w http.ResponseWriter, r *http.Request, req copilotgw.ChatContinuationRequest) {
	writer, ok := openai.NewSSEWriter(w)
	if !ok {
		openai.WriteError(w, openai.Internal("streaming unsupported by ResponseWriter"))
		return
	}
	ctx, cancel := requestContext(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	ch, err := s.gw.StreamContinueChatToolCalls(ctx, req)
	if err != nil {
		_ = writer.Data(openai.ErrorEnvelope{Error: errorObject(err)})
		_ = writer.Done()
		return
	}
	streamID := openai.NewID("chatcmpl_")
	created := openai.UnixNow()
	if err := writer.Data(openai.ChatCompletionChunk{ID: streamID, Object: openai.ObjectChatChunk, Created: created, Model: req.Model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Role: "assistant"}}}, IncludeUsage: req.IncludeUsageChunk}); err != nil {
		return
	}
	for ev := range ch {
		switch ev.Kind {
		case "delta":
			if err := writer.Data(openai.ChatCompletionChunk{ID: streamID, Object: openai.ObjectChatChunk, Created: created, Model: req.Model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Content: ev.Delta}}}, IncludeUsage: req.IncludeUsageChunk}); err != nil {
				return
			}
		case "result":
			if err := s.writeChatTerminalWithID(writer, streamID, created, req.Model, ev.Result, req.IncludeUsageChunk); err != nil {
				return
			}
		case "error":
			if err := writer.Data(openai.ErrorEnvelope{Error: errorObject(ev.Error)}); err != nil {
				return
			}
		}
	}
	_ = writer.Done()
}
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, req copilotgw.ChatRequest) {
	writer, ok := openai.NewSSEWriter(w)
	if !ok {
		openai.WriteError(w, openai.Internal("streaming unsupported by ResponseWriter"))
		return
	}
	ctx, cancel := requestContext(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	ch, err := s.gw.StreamChat(ctx, req)
	if err != nil {
		_ = writer.Data(openai.ErrorEnvelope{Error: errorObject(err)})
		_ = writer.Done()
		return
	}
	created := openai.UnixNow()
	if err := writer.Data(openai.ChatCompletionChunk{ID: req.OpenAIID, Object: openai.ObjectChatChunk, Created: created, Model: req.Model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Role: "assistant"}}}, IncludeUsage: req.IncludeUsageChunk}); err != nil {
		return
	}
	for ev := range ch {
		switch ev.Kind {
		case "delta":
			if err := writer.Data(openai.ChatCompletionChunk{ID: req.OpenAIID, Object: openai.ObjectChatChunk, Created: created, Model: req.Model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Content: ev.Delta}}}, IncludeUsage: req.IncludeUsageChunk}); err != nil {
				return
			}
		case "result":
			if err := s.writeChatTerminal(writer, ev.Result, req.IncludeUsageChunk); err != nil {
				return
			}
		case "error":
			if err := writer.Data(openai.ErrorEnvelope{Error: errorObject(ev.Error)}); err != nil {
				return
			}
		}
	}
	_ = writer.Done()
}
func (s *Server) writeChatStreamFromTurn(writer *openai.SSEWriter, turn *copilotgw.TurnResult, includeUsage bool) error {
	if err := writer.Data(openai.ChatCompletionChunk{ID: turn.ID, Object: openai.ObjectChatChunk, Created: turn.Created, Model: turn.Model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Role: "assistant"}}}, IncludeUsage: includeUsage}); err != nil {
		return err
	}
	if turn.Text != "" {
		if err := writer.Data(openai.ChatCompletionChunk{ID: turn.ID, Object: openai.ObjectChatChunk, Created: turn.Created, Model: turn.Model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Content: turn.Text}}}, IncludeUsage: includeUsage}); err != nil {
			return err
		}
	}
	if err := s.writeChatTerminal(writer, turn, includeUsage); err != nil {
		return err
	}
	return writer.Done()
}
func (s *Server) writeChatTerminal(writer *openai.SSEWriter, turn *copilotgw.TurnResult, includeUsage bool) error {
	return s.writeChatTerminalWithID(writer, turn.ID, turn.Created, turn.Model, turn, includeUsage)
}
func (s *Server) writeChatTerminalWithID(writer *openai.SSEWriter, id string, created int64, model string, turn *copilotgw.TurnResult, includeUsage bool) error {
	finish := turn.FinishReason
	if len(turn.ToolCalls) > 0 {
		deltas := make([]openai.ToolCallDelta, 0, len(turn.ToolCalls))
		for i, tc := range turn.ToolCalls {
			deltas = append(deltas, openai.ToolCallDelta{Index: i, ID: tc.ID, Type: "function", Function: &openai.ToolCallDeltaFunction{Name: tc.Function.Name, Arguments: tc.Function.Arguments}})
		}
		if err := writer.Data(openai.ChatCompletionChunk{ID: id, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{ToolCalls: deltas}}}, IncludeUsage: includeUsage}); err != nil {
			return err
		}
	}
	if err := writer.Data(openai.ChatCompletionChunk{ID: id, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{}, FinishReason: &finish}}, IncludeUsage: includeUsage}); err != nil {
		return err
	}
	if includeUsage {
		if err := writer.Data(openai.ChatCompletionChunk{ID: id, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{}, Usage: turn.Usage, IncludeUsage: true}); err != nil {
			return err
		}
	}
	return nil
}
func chatCompletionFromTurn(turn *copilotgw.TurnResult) openai.ChatCompletion {
	msg := openai.ChatMessage{Role: "assistant", Content: openai.NewTextContent(turn.Text), ToolCalls: turn.ToolCalls}
	if turn.Text == "" && len(turn.ToolCalls) > 0 {
		msg.Content = openai.Content{Present: true, IsNull: true}
	}
	return openai.ChatCompletion{ID: turn.ID, Object: openai.ObjectChatCompletion, Created: turn.Created, Model: turn.Model, Choices: []openai.ChatCompletionChoice{{Index: 0, Message: msg, FinishReason: turn.FinishReason}}, Usage: turn.Usage, SystemFingerprint: nil}
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
