package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
)

func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	var req openai.ResponsesRequest
	if err := decodeJSON(w, r, s.cfg.MaxRequestBodyBytes, &req); err != nil {
		WriteError(w, err)
		return
	}
	ctx, cancel := requestContext(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	gwReq, logFields, err := s.prepareResponseRequest(ctx, &req, openai.NewID("resp_"))
	if err != nil {
		WriteError(w, err)
		return
	}
	s.logGenerationStarted(r, "responses", req.Model, logFields.reasoningEffort, logFields.resolvedEffort, logFields.resolved, logFields.continuation)
	if req.Stream {
		s.streamResponses(w, r, gwReq)
		return
	}
	res, err := s.gw.CreateResponse(ctx, gwReq)
	if err != nil {
		WriteError(w, err)
		return
	}
	// The non-streaming body is the terminal event of the same sequence SSE and
	// WebSocket serialize, folded back into a response. It is not an independent
	// rendering of the gateway result.
	folder := &foldingResponseEventWriter{}
	result := foldResponseResult(ctx, folder, gwReq, s.cfg.MaxTurnOutputBytes, s.suppressReasoning(), res)
	if result.Err != nil {
		WriteError(w, result.Err)
		return
	}
	if folder.response == nil {
		WriteError(w, apierr.Internal("response stream produced no terminal response"))
		return
	}
	writeJSON(w, http.StatusOK, folder.response)
}

// foldResponseResult replays a non-streaming gateway result through the shared
// Responses event sequence so the JSON body, the SSE stream and the WebSocket
// stream all describe the turn the same way.
func foldResponseResult(ctx context.Context, writer responseEventWriter, req copilotgw.ResponseRequest, maxOutputBytes int64, suppressReasoning bool, res *copilotgw.ResponseResult) responseStreamWriteResult {
	ch := make(chan copilotgw.ResponseStreamEvent, 1)
	ch <- copilotgw.ResponseStreamEvent{Kind: "response", Response: res.Response}
	close(ch)
	return writeResponseStreamEvents(ctx, writer, req, maxOutputBytes, suppressReasoning, ch)
}

// suppressReasoning reports whether the configured reasoning-emission policy
// hides reasoning from what this server writes. It is deliberately resolved
// here and nowhere deeper: the gateway always builds and persists a complete
// response, so this is a per-request rendering decision that can be flipped at
// any time without rewriting history.
func (s *Server) suppressReasoning() bool {
	return !openai.ResolveReasoningEmission(s.cfg.ReasoningEmission).Enabled()
}

type preparedResponseLogFields struct {
	reasoningEffort string
	resolvedEffort  string
	resolved        bool
	continuation    bool
}

func (s *Server) logResponsesToolSummary(ctx context.Context, tools []toolcatalog.NormalizedTool) {
	if s.log == nil {
		return
	}
	counts := map[string]int{}
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		counts[string(tool.Kind)]++
		switch tool.Kind {
		case toolcatalog.ToolKindNamespace:
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

// logUnhonoredResponseControls is the Responses counterpart of
// logUnhonoredChatControls: these controls are accepted so real clients work,
// but the Copilot SDK cannot act on them, so record them at debug level.
func (s *Server) logUnhonoredResponseControls(ctx context.Context, req *openai.ResponsesRequest) {
	if !s.debugEnabled(ctx) {
		return
	}
	if tokens, ok := openai.ResponsesMaxOutputTokens(req); ok {
		s.debugStream(ctx, "max output tokens is not forwarded to the Copilot SDK", "surface", "responses", "max_output_tokens", tokens)
	}
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		s.debugStream(ctx, "parallel_tool_calls=false is not enforced by the Copilot SDK", "surface", "responses")
	}
	if ignored := openai.IgnoredResponsesToolTypes(req.Tools); len(ignored) > 0 {
		s.debugStream(ctx, "hosted Responses tools were dropped", "surface", "responses", "tool_types", ignored)
	}
	s.logUnhonoredToolChoice(ctx, "responses", req.ToolChoice)
}

func (s *Server) prepareResponseRequest(ctx context.Context, req *openai.ResponsesRequest, responseID string) (copilotgw.ResponseRequest, preparedResponseLogFields, error) {
	selector, err := openai.ParseModelSelector(req.Model)
	if err != nil {
		return copilotgw.ResponseRequest{}, preparedResponseLogFields{}, err
	}
	mergedEffort, err := openai.MergeReasoningEffort(selector, req.ReasoningEffort, "reasoning_effort")
	if err != nil {
		return copilotgw.ResponseRequest{}, preparedResponseLogFields{}, err
	}
	req.Model = selector.Model
	req.ReasoningEffort = mergedEffort
	if err := openai.ValidateResponsesRequest(req, s.cfg.StrictCompat); err != nil {
		return copilotgw.ResponseRequest{}, preparedResponseLogFields{}, err
	}
	reasoningEffort := openai.ResponsesReasoningEffort(req)
	s.logUnhonoredResponseControls(ctx, req)
	normalizedTools, err := openai.NormalizeResponsesToolsWithMode(req.Tools, s.cfg.StrictCompat)
	if err != nil {
		return copilotgw.ResponseRequest{}, preparedResponseLogFields{}, err
	}
	if s.cfg.LogContent {
		s.logResponsesToolSummary(ctx, normalizedTools)
	}
	parsedInput, err := parseResponsesInputOnce(req.Input)
	if err != nil {
		return copilotgw.ResponseRequest{}, preparedResponseLogFields{}, err
	}
	input := parsedInput.input
	outputs := parsedInput.outputs
	inputInstructions := parsedInput.instructions
	var fallbackInput openai.PromptContent
	fallbackInstructions := ""
	fallbackAvailable := false
	if len(outputs) > 0 && req.PreviousResponseID == "" {
		fallbackInput = parsedInput.fallbackInput
		fallbackInstructions = parsedInput.fallbackInstructions
		fallbackAvailable = parsedInput.fallbackAvailable
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
	_, toolsSet := req.Raw["tools"]
	gwReq := copilotgw.ResponseRequest{
		ResponseID: responseID,
		// Stamp the creation time once, here, so response.created, the terminal
		// response.completed and the stored record all report the same instant.
		CreatedAt:                          openai.UnixNow(),
		Model:                              req.Model,
		Instructions:                       combineInstructions(req.Instructions, inputInstructions),
		Input:                              input,
		ToolOutputs:                        outputs,
		FunctionOutputFallbackInput:        fallbackInput,
		FunctionOutputFallbackInstructions: fallbackInstructions,
		FunctionOutputFallbackAvailable:    fallbackAvailable,
		PreviousResponseID:                 req.PreviousResponseID,
		Tools:                              normalizedTools,
		ToolsSet:                           toolsSet,
		ToolChoiceNone:                     openai.ToolChoiceNone(req.ToolChoice),
		Store:                              store,
		StoreSet:                           storeSet,
		ReasoningEffort:                    reasoningEffort,
		DefaultReasoningEffort:             s.cfg.DefaultReasoningEffort,
		ResolvedReasoningEffort:            resolvedEffort,
		ReasoningEffortResolved:            resolved,
	}
	return gwReq, preparedResponseLogFields{reasoningEffort: reasoningEffort, resolvedEffort: resolvedEffort, resolved: resolved, continuation: continuation}, nil
}

func (s *Server) streamResponses(w http.ResponseWriter, r *http.Request, req copilotgw.ResponseRequest) {
	ctx, cancel := requestContext(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	// Do not commit SSE headers until gateway preflight succeeds.
	ch, err := s.gw.StreamResponse(ctx, req)
	if err != nil {
		WriteError(w, err)
		return
	}
	writer, ok := NewSSEWriter(w)
	if !ok {
		WriteError(w, apierr.Internal("streaming unsupported by ResponseWriter"))
		return
	}
	responseWriter := newResponseStreamEncoder(newLoggedResponseEventWriter(s, ctx, sseResponseEventTransport{writer: writer}))
	// Owning the encoder here keeps sequence numbers continuous if a panic
	// forces the terminal response.failed frame below.
	setStreamFailureWriter(w, func(failure error) {
		if writeResponseFailedEvent(responseWriter, req, failure, nil, "") == nil {
			_ = s.writeSSEDone(ctx, writer, "stream_kind", "responses")
		}
	})
	result := writeResponseStreamEvents(ctx, responseWriter, req, s.cfg.MaxTurnOutputBytes, s.suppressReasoning(), ch)
	if !result.WriteFailed {
		_ = s.writeSSEDone(ctx, writer, "stream_kind", "responses")
	}
}
func (s *Server) getResponse(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/responses/")
	if id == "" || strings.Contains(id, "/") {
		WriteError(w, apierr.NotFound("response not found", "not_found"))
		return
	}
	ctx, cancel := requestContext(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	resp, err := s.gw.GetResponse(ctx, id)
	if err != nil {
		WriteError(w, err)
		return
	}
	// Stored records are complete; the emission policy only decides what this
	// read renders, so it is applied here and never on the way in.
	writeJSON(w, http.StatusOK, filterResponseReasoning(resp, s.suppressReasoning()))
}
func (s *Server) deleteResponse(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/v1/responses/")
	if id == "" || strings.Contains(id, "/") {
		WriteError(w, apierr.NotFound("response not found", "not_found"))
		return
	}
	ctx, cancel := requestContext(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	if err := s.gw.DeleteResponse(ctx, id); err != nil {
		WriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "object": "response.deleted", "deleted": true})
}
