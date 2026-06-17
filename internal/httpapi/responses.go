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
	writeJSON(w, http.StatusOK, filterResponseReasoning(res.Response, gwReq.SuppressReasoning))
}

type preparedResponseLogFields struct {
	reasoningEffort string
	resolvedEffort  string
	resolved        bool
	continuation    bool
}

func (s *Server) logResponsesToolSummary(ctx context.Context, tools []openai.NormalizedTool) {
	if s.log == nil {
		return
	}
	counts := map[string]int{}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		counts[string(tool.Kind)]++
		switch tool.Kind {
		case openai.ToolKindNamespace:
			names = append(names, string(tool.Kind)+":"+tool.Name)
			for _, child := range tool.Children {
				names = append(names, "function:"+tool.Name+"."+child.Name)
			}
		default:
			names = append(names, string(tool.Kind)+":"+tool.Name)
		}
	}
	s.log.DebugContext(ctx, "responses tool catalog", "tool_type_counts", counts, "tools", names)
}

func (s *Server) prepareResponseRequest(ctx context.Context, req *openai.ResponsesRequest, responseID string) (copilotgw.ResponseRequest, preparedResponseLogFields, error) {
	reasoningEffort := openai.ResponsesReasoningEffort(req)
	if err := openai.ValidateResponsesRequest(req, s.cfg.StrictCompat); err != nil {
		return copilotgw.ResponseRequest{}, preparedResponseLogFields{}, err
	}
	normalizedTools, err := openai.NormalizeResponsesToolsWithMode(req.Tools, s.cfg.StrictCompat)
	if err != nil {
		return copilotgw.ResponseRequest{}, preparedResponseLogFields{}, err
	}
	if s.cfg.LogContent {
		s.logResponsesToolSummary(ctx, normalizedTools)
	}
	input, outputs, inputInstructions, err := parseResponsesInput(req.Input)
	if err != nil {
		return copilotgw.ResponseRequest{}, preparedResponseLogFields{}, err
	}
	var fallbackInput openai.PromptContent
	fallbackInstructions := ""
	fallbackAvailable := false
	if len(outputs) > 0 && req.PreviousResponseID == "" {
		fallbackInput, fallbackInstructions, fallbackAvailable, err = parseResponsesFallbackInput(req.Input)
		if err != nil {
			return copilotgw.ResponseRequest{}, preparedResponseLogFields{}, err
		}
		if fallbackAvailable {
			fallbackInstructions = combineInstructions(req.Instructions, fallbackInstructions)
		}
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
	gwReq := copilotgw.ResponseRequest{
		ResponseID:                         responseID,
		Model:                              req.Model,
		Instructions:                       combineInstructions(req.Instructions, inputInstructions),
		Input:                              input,
		ToolOutputs:                        outputs,
		FunctionOutputFallbackInput:        fallbackInput,
		FunctionOutputFallbackInstructions: fallbackInstructions,
		FunctionOutputFallbackAvailable:    fallbackAvailable,
		PreviousResponseID:                 req.PreviousResponseID,
		Tools:                              normalizedTools,
		ToolChoiceNone:                     openai.ToolChoiceNone(req.ToolChoice),
		Store:                              store,
		StoreSet:                           storeSet,
		ReasoningEffort:                    reasoningEffort,
		DefaultReasoningEffort:             s.cfg.DefaultReasoningEffort,
		ResolvedReasoningEffort:            resolvedEffort,
		ReasoningEffortResolved:            resolved,
		SuppressReasoning:                  !openai.ResolveReasoningEmission(s.cfg.ReasoningEmission).Enabled(),
	}
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
	responseWriter := sseResponseEventWriter{server: s, ctx: ctx, writer: writer}
	if err != nil {
		_ = writeResponseFailedEvent(newResponseStreamEncoder(responseWriter), req, err)
		_ = s.writeSSEDone(ctx, writer, "stream_kind", "responses")
		return
	}
	_ = writeResponseStreamEvents(ctx, responseWriter, req, ch)
	_ = s.writeSSEDone(ctx, writer, "stream_kind", "responses")
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
	writeJSON(w, http.StatusOK, filterResponseReasoning(resp, !openai.ResolveReasoningEmission(s.cfg.ReasoningEmission).Enabled()))
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
