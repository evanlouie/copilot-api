package httpapi

import (
	"net/http"
	"strings"

	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

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
	gwReq := copilotgw.ResponseRequest{ResponseID: openai.NewID("resp_"), Model: req.Model, Instructions: combineInstructions(req.Instructions, inputInstructions), Input: input, FunctionOutputs: outputs, PreviousResponseID: req.PreviousResponseID, Tools: openai.SupportedTools(req.Tools), ToolChoiceNone: openai.ToolChoiceNone(req.ToolChoice), Store: store, StoreSet: storeSet, ReasoningEffort: reasoningEffort, DefaultReasoningEffort: s.cfg.DefaultReasoningEffort}
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
		_ = s.writeResponseFailed(writer, req, err)
		_ = writer.Done()
		return
	}
	var previous *string
	if req.PreviousResponseID != "" {
		previous = &req.PreviousResponseID
	}
	created := openai.UnixNow()
	initial := &openai.Response{ID: req.ResponseID, Object: openai.ObjectResponse, CreatedAt: created, Status: "in_progress", Model: req.Model, Instructions: req.Instructions, Output: []openai.ResponseOutputItem{}, OutputText: "", ParallelToolCalls: true, PreviousResponseID: previous, Store: req.Store, Error: nil, IncompleteDetails: nil}
	if err := writer.Event("response.created", openai.ResponseStreamEvent{Type: "response.created", Response: initial}); err != nil {
		return
	}
	if err := writer.Event("response.in_progress", openai.ResponseStreamEvent{Type: "response.in_progress", Response: initial}); err != nil {
		return
	}
	messageID := openai.NewID("msg_")
	messageStarted := false
	messageDone := false
	var messageText strings.Builder
	for ev := range ch {
		switch ev.Kind {
		case "delta":
			zero := 0
			if !messageStarted {
				item := openai.ResponseOutputItem{ID: messageID, Type: "message", Status: "in_progress", Role: "assistant", Content: []openai.ResponseText{}}
				if err := writer.Event("response.output_item.added", openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &zero, Item: &item}); err != nil {
					return
				}
				messageStarted = true
			}
			messageText.WriteString(ev.Delta)
			if err := writer.Event("response.output_text.delta", openai.ResponseStreamEvent{Type: "response.output_text.delta", OutputIndex: &zero, ContentIndex: &zero, ItemID: messageID, Delta: ev.Delta}); err != nil {
				return
			}
		case "response":
			if messageStarted {
				text := messageText.String()
				zero := 0
				if err := writer.Event("response.output_text.done", openai.ResponseStreamEvent{Type: "response.output_text.done", OutputIndex: &zero, ContentIndex: &zero, ItemID: messageID, Text: text}); err != nil {
					return
				}
				messageDone = true
				if item, idx := streamedMessageItem(ev.Response, messageID, text); item != nil {
					if err := writer.Event("response.output_item.done", openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: item}); err != nil {
						return
					}
				}
			}
			if err := s.writeResponseOutputEvents(writer, ev.Response); err != nil {
				return
			}
			if err := writer.Event("response.completed", openai.ResponseStreamEvent{Type: "response.completed", Response: ev.Response}); err != nil {
				return
			}
		case "error":
			if messageStarted && !messageDone {
				zero := 0
				if err := writer.Event("response.output_text.done", openai.ResponseStreamEvent{Type: "response.output_text.done", OutputIndex: &zero, ContentIndex: &zero, ItemID: messageID, Text: messageText.String()}); err != nil {
					return
				}
			}
			if err := s.writeResponseFailed(writer, req, ev.Error); err != nil {
				return
			}
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
func (s *Server) writeResponseOutputEvents(writer *openai.SSEWriter, resp *openai.Response) error {
	if resp == nil {
		return nil
	}
	for i := range resp.Output {
		item := resp.Output[i]
		if item.Type != "function_call" {
			continue
		}
		idx := i
		if err := writer.Event("response.output_item.added", openai.ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &idx, Item: &item}); err != nil {
			return err
		}
		if item.Arguments != "" {
			if err := writer.Event("response.function_call_arguments.delta", openai.ResponseStreamEvent{Type: "response.function_call_arguments.delta", OutputIndex: &idx, ItemID: item.ID, Delta: item.Arguments}); err != nil {
				return err
			}
		}
		if err := writer.Event("response.function_call_arguments.done", openai.ResponseStreamEvent{Type: "response.function_call_arguments.done", OutputIndex: &idx, ItemID: item.ID, Arguments: item.Arguments}); err != nil {
			return err
		}
		if err := writer.Event("response.output_item.done", openai.ResponseStreamEvent{Type: "response.output_item.done", OutputIndex: &idx, Item: &item}); err != nil {
			return err
		}
	}
	return nil
}
func (s *Server) writeResponseFailed(writer *openai.SSEWriter, req copilotgw.ResponseRequest, err error) error {
	obj := errorObject(err)
	var previous *string
	if req.PreviousResponseID != "" {
		previous = &req.PreviousResponseID
	}
	resp := &openai.Response{ID: req.ResponseID, Object: openai.ObjectResponse, CreatedAt: openai.UnixNow(), Status: "failed", Model: req.Model, Instructions: req.Instructions, Output: []openai.ResponseOutputItem{}, OutputText: "", ParallelToolCalls: true, PreviousResponseID: previous, Store: req.Store, Error: obj, IncompleteDetails: nil}
	return writer.Event("response.failed", openai.ResponseStreamEvent{Type: "response.failed", Response: resp, Error: &obj})
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
