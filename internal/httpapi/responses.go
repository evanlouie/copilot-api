package httpapi

import (
	"context"
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
	ctx, cancel := requestContext(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	gwReq, logFields, err := s.prepareResponseRequest(ctx, &req, openai.NewID("resp_"))
	if err != nil {
		openai.WriteError(w, err)
		return
	}
	s.logGenerationStarted(r, "responses", req.Model, logFields.reasoningEffort, logFields.resolvedEffort, logFields.resolved, logFields.continuation)
	if req.Stream {
		s.streamResponses(w, r, gwReq)
		return
	}
	res, err := s.gw.CreateResponse(ctx, gwReq)
	if err != nil {
		openai.WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res.Response)
}

type preparedResponseLogFields struct {
	reasoningEffort string
	resolvedEffort  string
	resolved        bool
	continuation    bool
}

func (s *Server) prepareResponseRequest(ctx context.Context, req *openai.ResponsesRequest, responseID string) (copilotgw.ResponseRequest, preparedResponseLogFields, error) {
	reasoningEffort := openai.ResponsesReasoningEffort(req)
	if err := openai.ValidateResponsesRequest(req, s.cfg.StrictCompat); err != nil {
		return copilotgw.ResponseRequest{}, preparedResponseLogFields{}, err
	}
	input, outputs, inputInstructions, err := parseResponsesInput(req.Input)
	if err != nil {
		return copilotgw.ResponseRequest{}, preparedResponseLogFields{}, err
	}
	store := true
	storeSet := req.Store != nil
	if req.Store != nil {
		store = *req.Store
	}
	continuation := len(outputs) > 0
	resolvedEffort := ""
	resolved := false
	if !continuation {
		resolvedEffort, resolved, err = s.resolveGenerationReasoningEffort(ctx, req.Model, reasoningEffort)
		if err != nil {
			return copilotgw.ResponseRequest{}, preparedResponseLogFields{}, err
		}
	}
	gwReq := copilotgw.ResponseRequest{ResponseID: responseID, Model: req.Model, Instructions: combineInstructions(req.Instructions, inputInstructions), Input: input, FunctionOutputs: outputs, PreviousResponseID: req.PreviousResponseID, Tools: openai.SupportedTools(req.Tools), ToolChoiceNone: openai.ToolChoiceNone(req.ToolChoice), Store: store, StoreSet: storeSet, ReasoningEffort: reasoningEffort, DefaultReasoningEffort: s.cfg.DefaultReasoningEffort, ResolvedReasoningEffort: resolvedEffort, ReasoningEffortResolved: resolved}
	return gwReq, preparedResponseLogFields{reasoningEffort: reasoningEffort, resolvedEffort: resolvedEffort, resolved: resolved, continuation: continuation}, nil
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
		_ = writeResponseFailedEvent(newResponseStreamEncoder(sseResponseEventWriter{writer: writer}), req, err)
		_ = writer.Done()
		return
	}
	_ = writeResponseStreamEvents(ctx, sseResponseEventWriter{writer: writer}, req, ch)
	_ = writer.Done()
}
func streamedMessageItem(resp *openai.Response, id, text string, insertIndex int) (*openai.ResponseOutputItem, int) {
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
	// Insert the message at the same output index used for the streamed
	// output_item.added event so a leading reasoning item keeps index 0 and the
	// message follows it.
	if insertIndex < 0 {
		insertIndex = 0
	}
	if insertIndex > len(resp.Output) {
		insertIndex = len(resp.Output)
	}
	resp.Output = append(resp.Output, openai.ResponseOutputItem{})
	copy(resp.Output[insertIndex+1:], resp.Output[insertIndex:])
	resp.Output[insertIndex] = item
	return &resp.Output[insertIndex], insertIndex
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
