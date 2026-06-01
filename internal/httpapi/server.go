package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/observability"
	"github.com/evanlouie/copilot-api/internal/openai"
)

type Server struct {
	cfg config.Config
	gw  copilotgw.Gateway
	log *slog.Logger
	mux *http.ServeMux
}

func New(cfg config.Config, gw copilotgw.Gateway, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, gw: gw, log: log, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	h = s.authMiddleware(h)
	h = recoverMiddleware(s.log, h)
	h = requestLoggingMiddleware(s.log, h)
	h = observability.RequestIDMiddleware(h)
	return h
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.health)
	s.mux.HandleFunc("GET /readyz", s.ready)
	s.mux.HandleFunc("GET /v1/models", s.models)
	s.mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	s.mux.HandleFunc("POST /v1/responses", s.responses)
	s.mux.HandleFunc("GET /v1/responses/", s.getResponse)
	s.mux.HandleFunc("DELETE /v1/responses/", s.deleteResponse)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "time": time.Now().UTC().Format(time.RFC3339Nano)})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.gw.Ready(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	models, err := s.gw.ListModels(r.Context())
	if err != nil {
		openai.WriteError(w, openai.Upstream(err.Error()))
		return
	}
	out := openai.ModelList{Object: openai.ObjectList}
	now := openai.UnixNow()
	for _, m := range models {
		out.Data = append(out.Data, openai.Model{ID: m.ID, Object: openai.ObjectModel, Created: now, OwnedBy: "github-copilot", Meta: m.Metadata})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	var req openai.ChatCompletionRequest
	if err := decodeJSON(w, r, s.cfg.MaxRequestBodyBytes, &req); err != nil {
		openai.WriteError(w, err)
		return
	}
	setRequestLogModel(r, req.Model)
	setRequestLogReasoningEffort(r, req.ReasoningEffort)
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
		outputs, err := trailingToolOutputs(messages)
		if err != nil {
			openai.WriteError(w, err)
			return
		}
		if req.Stream {
			s.streamChatContinuation(w, r, req.Model, outputs, req.StreamOptions != nil && req.StreamOptions.IncludeUsage)
			return
		}
		turn, err := s.gw.ContinueChatToolCalls(ctx, req.Model, outputs)
		if err != nil {
			openai.WriteError(w, err)
			return
		}
		turn.ID = openai.NewID("chatcmpl_")
		turn.Created = openai.UnixNow()
		writeJSON(w, http.StatusOK, chatCompletionFromTurn(turn))
		return
	}
	if len(messages) == 0 || messages[len(messages)-1].Role != "user" {
		openai.WriteError(w, openai.InvalidRequest("ordinary Chat Completions requests must end with a user message", "messages"))
		return
	}
	chatReq := copilotgw.ChatRequest{OpenAIID: openai.NewID("chatcmpl_"), Model: req.Model, Instructions: instructions, History: messages[:len(messages)-1], FinalUser: messages[len(messages)-1], Tools: req.Tools, ToolChoiceNone: openai.ToolChoiceNone(req.ToolChoice), ReasoningEffort: req.ReasoningEffort, IncludeUsageChunk: req.StreamOptions != nil && req.StreamOptions.IncludeUsage}
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

func (s *Server) streamChatContinuation(w http.ResponseWriter, r *http.Request, model string, outputs map[string]string, includeUsage bool) {
	writer, ok := openai.NewSSEWriter(w)
	if !ok {
		openai.WriteError(w, openai.Internal("streaming unsupported by ResponseWriter"))
		return
	}
	ctx, cancel := requestContext(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	ch, err := s.gw.StreamContinueChatToolCalls(ctx, model, outputs)
	if err != nil {
		_ = writer.Data(openai.ErrorEnvelope{Error: openai.ErrorObject{Message: err.Error(), Type: "invalid_request_error"}})
		_ = writer.Done()
		return
	}
	streamID := openai.NewID("chatcmpl_")
	created := openai.UnixNow()
	_ = writer.Data(openai.ChatCompletionChunk{ID: streamID, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Role: "assistant"}}}})
	for ev := range ch {
		switch ev.Kind {
		case "delta":
			_ = writer.Data(openai.ChatCompletionChunk{ID: streamID, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Content: ev.Delta}}}})
		case "result":
			s.writeChatTerminalWithID(writer, streamID, created, model, ev.Result, includeUsage)
		case "error":
			_ = writer.Data(openai.ErrorEnvelope{Error: openai.ErrorObject{Message: ev.Error.Error(), Type: "server_error"}})
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
		_ = writer.Data(openai.ErrorEnvelope{Error: openai.ErrorObject{Message: err.Error(), Type: "server_error"}})
		_ = writer.Done()
		return
	}
	created := openai.UnixNow()
	_ = writer.Data(openai.ChatCompletionChunk{ID: req.OpenAIID, Object: openai.ObjectChatChunk, Created: created, Model: req.Model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Role: "assistant"}}}})
	for ev := range ch {
		switch ev.Kind {
		case "delta":
			_ = writer.Data(openai.ChatCompletionChunk{ID: req.OpenAIID, Object: openai.ObjectChatChunk, Created: created, Model: req.Model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Content: ev.Delta}}}})
		case "result":
			s.writeChatTerminal(writer, ev.Result, req.IncludeUsageChunk)
		case "error":
			_ = writer.Data(openai.ErrorEnvelope{Error: openai.ErrorObject{Message: ev.Error.Error(), Type: "server_error"}})
		}
	}
	_ = writer.Done()
}

func (s *Server) writeChatStreamFromTurn(writer *openai.SSEWriter, turn *copilotgw.TurnResult, includeUsage bool) {
	_ = writer.Data(openai.ChatCompletionChunk{ID: turn.ID, Object: openai.ObjectChatChunk, Created: turn.Created, Model: turn.Model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Role: "assistant"}}}})
	if turn.Text != "" {
		_ = writer.Data(openai.ChatCompletionChunk{ID: turn.ID, Object: openai.ObjectChatChunk, Created: turn.Created, Model: turn.Model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Content: turn.Text}}}})
	}
	s.writeChatTerminal(writer, turn, includeUsage)
	_ = writer.Done()
}

func (s *Server) writeChatTerminal(writer *openai.SSEWriter, turn *copilotgw.TurnResult, includeUsage bool) {
	s.writeChatTerminalWithID(writer, turn.ID, turn.Created, turn.Model, turn, includeUsage)
}

func (s *Server) writeChatTerminalWithID(writer *openai.SSEWriter, id string, created int64, model string, turn *copilotgw.TurnResult, includeUsage bool) {
	finish := turn.FinishReason
	if len(turn.ToolCalls) > 0 {
		deltas := make([]openai.ToolCallDelta, 0, len(turn.ToolCalls))
		for i, tc := range turn.ToolCalls {
			deltas = append(deltas, openai.ToolCallDelta{Index: i, ID: tc.ID, Type: "function", Function: &openai.ToolCallDeltaFunction{Name: tc.Function.Name, Arguments: tc.Function.Arguments}})
		}
		_ = writer.Data(openai.ChatCompletionChunk{ID: id, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{ToolCalls: deltas}}}})
	}
	_ = writer.Data(openai.ChatCompletionChunk{ID: id, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{}, FinishReason: &finish}}})
	if includeUsage && turn.Usage != nil {
		_ = writer.Data(openai.ChatCompletionChunk{ID: id, Object: openai.ObjectChatChunk, Created: created, Model: model, Choices: []openai.ChatChunkChoice{}, Usage: turn.Usage})
	}
}

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	var req openai.ResponsesRequest
	if err := decodeJSON(w, r, s.cfg.MaxRequestBodyBytes, &req); err != nil {
		openai.WriteError(w, err)
		return
	}
	setRequestLogModel(r, req.Model)
	reasoningEffort := openai.ResponsesReasoningEffort(&req)
	setRequestLogReasoningEffort(r, reasoningEffort)
	if err := openai.ValidateResponsesRequest(&req, s.cfg.StrictCompat); err != nil {
		openai.WriteError(w, err)
		return
	}
	input, outputs, inputInstructions, err := parseResponsesInput(req.Input)
	if err != nil {
		openai.WriteError(w, err)
		return
	}
	store := true
	storeSet := req.Store != nil
	if req.Store != nil {
		store = *req.Store
	}
	gwReq := copilotgw.ResponseRequest{ResponseID: openai.NewID("resp_"), Model: req.Model, Instructions: combineInstructions(req.Instructions, inputInstructions), Input: input, FunctionOutputs: outputs, PreviousResponseID: req.PreviousResponseID, Tools: openai.SupportedTools(req.Tools), ToolChoiceNone: openai.ToolChoiceNone(req.ToolChoice), Store: store, StoreSet: storeSet, ReasoningEffort: reasoningEffort}
	if req.Stream {
		s.streamResponses(w, r, gwReq)
		return
	}
	ctx, cancel := requestContext(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	res, err := s.gw.CreateResponse(ctx, gwReq)
	if err != nil {
		openai.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res.Response)
}

func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, req copilotgw.ResponseRequest) {
	writer, ok := openai.NewSSEWriter(w)
	if !ok {
		openai.WriteError(w, openai.Internal("streaming unsupported by ResponseWriter"))
		return
	}
	ctx, cancel := requestContext(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	ch, err := s.gw.StreamResponse(ctx, req)
	if err != nil {
		_ = writer.Event("error", openai.ResponseStreamEvent{Type: "error", Error: &openai.ErrorObject{Message: err.Error(), Type: "server_error"}})
		_ = writer.Done()
		return
	}
	var previous *string
	if req.PreviousResponseID != "" {
		previous = &req.PreviousResponseID
	}
	_ = writer.Event("response.created", openai.ResponseStreamEvent{Type: "response.created", Response: &openai.Response{ID: req.ResponseID, Object: openai.ObjectResponse, CreatedAt: openai.UnixNow(), Status: "in_progress", Model: req.Model, Instructions: req.Instructions, Output: []openai.ResponseOutputItem{}, OutputText: "", ParallelToolCalls: true, PreviousResponseID: previous, Store: req.Store, Error: nil, IncompleteDetails: nil}})
	messageID := openai.NewID("msg_")
	messageStarted := false
	var messageText strings.Builder
	for ev := range ch {
		switch ev.Kind {
		case "delta":
			zero := 0
			if !messageStarted {
				item := openai.ResponseOutputItem{ID: messageID, Type: "message", Status: "in_progress", Role: "assistant", Content: []openai.ResponseText{}}
				_ = writer.Event("response.output_item.added", openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &zero, Item: &item})
				messageStarted = true
			}
			messageText.WriteString(ev.Delta)
			_ = writer.Event("response.output_text.delta", openai.ResponseStreamEvent{Type: "response.output_text.delta", OutputIndex: &zero, ContentIndex: &zero, ItemID: messageID, Delta: ev.Delta})
		case "response":
			if messageStarted {
				if item, idx := streamedMessageItem(ev.Response, messageID, messageText.String()); item != nil {
					_ = writer.Event("response.output_item.done", openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: item})
				}
			}
			s.writeResponseOutputEvents(writer, ev.Response)
			_ = writer.Event("response.completed", openai.ResponseStreamEvent{Type: "response.completed", Response: ev.Response})
		case "error":
			_ = writer.Event("error", openai.ResponseStreamEvent{Type: "error", Error: &openai.ErrorObject{Message: ev.Error.Error(), Type: "server_error"}})
		}
	}
	_ = writer.Done()
}

func streamedMessageItem(resp *openai.Response, id, text string) (*openai.ResponseOutputItem, int) {
	if resp == nil {
		return nil, 0
	}
	if resp.OutputText == "" {
		resp.OutputText = text
	}
	item := openai.ResponseOutputItem{ID: id, Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: resp.OutputText}}}
	for i := range resp.Output {
		if resp.Output[i].Type == "message" {
			item = resp.Output[i]
			item.ID = id
			item.Status = "completed"
			if len(item.Content) == 0 {
				item.Content = []openai.ResponseText{{Type: "output_text", Text: resp.OutputText}}
			}
			resp.Output = append(resp.Output[:i], resp.Output[i+1:]...)
			break
		}
	}
	resp.Output = append([]openai.ResponseOutputItem{item}, resp.Output...)
	return &resp.Output[0], 0
}

func (s *Server) writeResponseOutputEvents(writer *openai.SSEWriter, resp *openai.Response) {
	if resp == nil {
		return
	}
	for i := range resp.Output {
		item := resp.Output[i]
		if item.Type != "function_call" {
			continue
		}
		idx := i
		_ = writer.Event("response.output_item.added", openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &idx, Item: &item})
		if item.Arguments != "" {
			_ = writer.Event("response.function_call_arguments.delta", openai.ResponseStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: &idx, ItemID: item.ID, Delta: item.Arguments})
		}
		_ = writer.Event("response.function_call_arguments.done", openai.ResponseStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: &idx, ItemID: item.ID, Arguments: item.Arguments})
		_ = writer.Event("response.output_item.done", openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: &item})
	}
}

func (s *Server) getResponse(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/responses/")
	if id == "" || strings.Contains(id, "/") {
		openai.WriteError(w, openai.NotFound("response not found", "not_found"))
		return
	}
	resp, err := s.gw.GetResponse(r.Context(), id)
	if err != nil {
		openai.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) deleteResponse(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/responses/")
	if id == "" || strings.Contains(id, "/") {
		openai.WriteError(w, openai.NotFound("response not found", "not_found"))
		return
	}
	if err := s.gw.DeleteResponse(r.Context(), id); err != nil {
		openai.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "object": "response.deleted", "deleted": true})
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

func parseResponsesInput(raw json.RawMessage) (openai.PromptContent, map[string]string, string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return openai.PromptContent{Text: s}, nil, "", nil
	}
	var items []openai.ResponseInputItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return openai.PromptContent{}, nil, "", openai.InvalidRequest("input must be a string or an array of response input items", "input")
	}
	if responsesInputIsToolContinuation(items) {
		outputs := map[string]string{}
		for i, item := range items {
			out, err := parseFunctionOutputItem(item, i)
			if err != nil {
				return openai.PromptContent{}, nil, "", err
			}
			if _, exists := outputs[item.CallID]; exists {
				return openai.PromptContent{}, nil, "", openai.InvalidRequest("duplicate function_call_output call_id", fmt.Sprintf("input.%d.call_id", i))
			}
			outputs[item.CallID] = out
		}
		return openai.PromptContent{}, outputs, "", nil
	}

	transcriptMode := responsesInputNeedsTranscript(items)
	var prompt openai.PromptContent
	var instructions []string
	var transcript []string
	for i, item := range items {
		switch item.Type {
		case "function_call_output":
			out, err := parseFunctionOutputItem(item, i)
			if err != nil {
				return openai.PromptContent{}, nil, "", err
			}
			transcript = append(transcript, "Function output "+item.CallID+":\n"+out)
		case "message", "":
			role := item.Role
			if role == "" {
				role = "user"
			}
			switch role {
			case "system", "developer":
				text, err := item.Content.Text()
				if err != nil {
					return openai.PromptContent{}, nil, "", openai.InvalidRequest(err.Error(), fmt.Sprintf("input.%d.content", i))
				}
				if strings.TrimSpace(text) != "" {
					instructions = append(instructions, responseRoleLabel(role)+":\n"+text)
				}
			case "user":
				part, err := item.Content.Prompt()
				if err != nil {
					return openai.PromptContent{}, nil, "", openai.InvalidRequest(err.Error(), fmt.Sprintf("input.%d.content", i))
				}
				appendPromptPart(&prompt, part, transcriptMode, &transcript, "User")
			case "assistant":
				text, err := item.Content.Text()
				if err != nil {
					return openai.PromptContent{}, nil, "", openai.InvalidRequest(err.Error(), fmt.Sprintf("input.%d.content", i))
				}
				if strings.TrimSpace(text) != "" {
					transcript = append(transcript, "Assistant:\n"+text)
				}
			default:
				return openai.PromptContent{}, nil, "", openai.InvalidRequest("unsupported response input message role", fmt.Sprintf("input.%d.role", i))
			}
		case "reasoning":
			// Reasoning items may appear in Codex history. They are not user-visible prompt content.
		case "function_call":
			transcript = append(transcript, "Assistant function call "+item.Name+" "+item.CallID+":\n"+item.Arguments)
		case "custom_tool_call":
			transcript = append(transcript, "Assistant custom tool call "+item.Name+" "+item.CallID+":\n"+item.Input)
		default:
			return openai.PromptContent{}, nil, "", openai.InvalidRequest("unsupported response input item type", fmt.Sprintf("input.%d.type", i))
		}
	}
	if len(transcript) > 0 {
		if prompt.Text != "" {
			transcript = append(transcript, "User:\n"+prompt.Text)
			prompt.Text = ""
		}
		prompt.Text = strings.Join(transcript, "\n\n")
	}
	return prompt, nil, strings.Join(instructions, "\n\n"), nil
}

func responsesInputIsToolContinuation(items []openai.ResponseInputItem) bool {
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		if item.Type != "function_call_output" {
			return false
		}
	}
	return true
}

func responsesInputNeedsTranscript(items []openai.ResponseInputItem) bool {
	for _, item := range items {
		if item.Type == "function_call" || item.Type == "custom_tool_call" || item.Type == "function_call_output" {
			return true
		}
		if (item.Type == "message" || item.Type == "") && item.Role == "assistant" {
			return true
		}
	}
	return false
}

func parseFunctionOutputItem(item openai.ResponseInputItem, i int) (string, error) {
	if item.CallID == "" {
		return "", openai.InvalidRequest("function_call_output items require call_id", fmt.Sprintf("input.%d.call_id", i))
	}
	out, err := outputRawToString(item.Output)
	if err != nil {
		return "", openai.InvalidRequest(err.Error(), fmt.Sprintf("input.%d.output", i))
	}
	return out, nil
}

func responseRoleLabel(role string) string {
	switch role {
	case "system":
		return "System"
	case "developer":
		return "Developer"
	default:
		return role
	}
}

func appendPromptPart(prompt *openai.PromptContent, part openai.PromptContent, transcriptMode bool, transcript *[]string, label string) {
	if transcriptMode {
		if strings.TrimSpace(part.Text) != "" {
			*transcript = append(*transcript, label+":\n"+part.Text)
		}
		prompt.Images = append(prompt.Images, part.Images...)
		return
	}
	if part.Text != "" {
		if prompt.Text != "" {
			prompt.Text += "\n"
		}
		prompt.Text += part.Text
	}
	prompt.Images = append(prompt.Images, part.Images...)
}

func combineInstructions(base, extra string) string {
	if strings.TrimSpace(extra) == "" {
		return base
	}
	if strings.TrimSpace(base) == "" {
		return extra
	}
	return base + "\n\n" + extra
}

func outputRawToString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	switch v.(type) {
	case map[string]any, []any:
		b, _ := json.Marshal(v)
		return string(b), nil
	default:
		return "", fmt.Errorf("function_call_output output must be string, JSON object, or JSON array")
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	body := r.Body
	if maxBytes > 0 {
		body = http.MaxBytesReader(w, r.Body, maxBytes)
	}
	defer r.Body.Close()
	dec := json.NewDecoder(body)
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return openai.InvalidRequest("invalid JSON request body: "+err.Error(), "body")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return openai.InvalidRequest("request body must contain a single JSON object", "body")
	}
	return nil
}

func requestContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") || s.cfg.APIKey == "" {
			next.ServeHTTP(w, r)
			return
		}
		expected := "Bearer " + s.cfg.APIKey
		if r.Header.Get("Authorization") != expected {
			openai.WriteError(w, openai.Unauthorized("invalid bearer token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

type requestLogMetadata struct {
	mu              sync.Mutex
	model           string
	reasoningEffort string
}

type requestLogMetadataKey struct{}

func requestLoggingMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		meta := &requestLogMetadata{}
		r = r.WithContext(context.WithValue(r.Context(), requestLogMetadataKey{}, meta))
		recorder := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		duration := time.Since(start)
		logger := observability.Logger(r.Context(), log)
		attrs := []any{
			"method", r.Method,
			"path", r.URL.EscapedPath(),
			"model", meta.Model(),
			"status", recorder.status,
			"bytes", recorder.bytes,
			"duration_ms", float64(duration.Microseconds()) / 1000.0,
			"remote_ip", remoteIP(r.RemoteAddr),
		}
		if reasoningEffort := meta.ReasoningEffort(); reasoningEffort != "" {
			attrs = append(attrs, "reasoning_effort", reasoningEffort)
		}
		if ua := r.UserAgent(); ua != "" {
			attrs = append(attrs, "user_agent", ua)
		}
		switch {
		case recorder.status >= 500:
			logger.Error("request completed", attrs...)
		case recorder.status >= 400:
			logger.Warn("request completed", attrs...)
		default:
			logger.Info("request completed", attrs...)
		}
	})
}

func setRequestLogModel(r *http.Request, model string) {
	meta, ok := r.Context().Value(requestLogMetadataKey{}).(*requestLogMetadata)
	if !ok || meta == nil {
		return
	}
	meta.SetModel(model)
}

func setRequestLogReasoningEffort(r *http.Request, reasoningEffort string) {
	meta, ok := r.Context().Value(requestLogMetadataKey{}).(*requestLogMetadata)
	if !ok || meta == nil {
		return
	}
	meta.SetReasoningEffort(reasoningEffort)
}

func (m *requestLogMetadata) SetModel(model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.model = model
}

func (m *requestLogMetadata) SetReasoningEffort(reasoningEffort string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reasoningEffort = reasoningEffort
}

func (m *requestLogMetadata) Model() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.model
}

func (m *requestLogMetadata) ReasoningEffort() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reasoningEffort
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.status = status
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *loggingResponseWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func remoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}

func recoverMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				observability.Logger(r.Context(), log).Error("panic in HTTP handler", "panic", v)
				openai.WriteError(w, openai.Internal("internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func asAPIError(err error) *openai.APIError {
	var api *openai.APIError
	if errors.As(err, &api) {
		return api
	}
	return openai.Internal(err.Error())
}
