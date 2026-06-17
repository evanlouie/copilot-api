package copilotgw

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"sync"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/google/uuid"
)

type WarmResponseSession struct {
	mu              sync.Mutex
	responseID      string
	sessionID       string
	model           string
	instructions    string
	reasoningEffort string
	tools           []openai.NormalizedTool
	toolChoiceNone  bool
	input           openai.PromptContent
	previous        *string
	store           bool
	retained        string
	session         *copilot.Session
	rt              *toolproxy.RequestTools
	events          chan copilot.SessionEvent
	disconnected    bool
}

func (w *WarmResponseSession) ResponseID() string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.responseID
}

func (w *WarmResponseSession) Disconnect() {
	if w == nil {
		return
	}
	w.mu.Lock()
	if w.disconnected {
		w.mu.Unlock()
		return
	}
	w.disconnected = true
	session := w.session
	w.mu.Unlock()
	if session != nil {
		_ = session.Disconnect()
	}
}

func (w *WarmResponseSession) use(req *ResponseRequest) (*copilot.Session, *toolproxy.RequestTools, chan copilot.SessionEvent, string, *string, bool) {
	if w == nil || req == nil || req.PreviousResponseID == "" {
		return nil, nil, nil, "", nil, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.disconnected || req.PreviousResponseID != w.responseID || req.Model != w.model {
		return nil, nil, nil, "", nil, false
	}
	if req.Instructions == "" {
		req.Instructions = w.instructions
	} else if req.Instructions != w.instructions {
		return nil, nil, nil, "", nil, false
	}
	requestReasoningEffort := cleanReasoningEffort(req.ReasoningEffort)
	if requestReasoningEffort == "" {
		req.ReasoningEffort = w.reasoningEffort
	} else if requestReasoningEffort != w.reasoningEffort {
		return nil, nil, nil, "", nil, false
	}
	if len(req.Tools) == 0 {
		req.Tools = append([]openai.NormalizedTool{}, w.tools...)
	} else if !responseToolsEqual(req.Tools, w.tools) {
		return nil, nil, nil, "", nil, false
	}
	if w.toolChoiceNone {
		req.ToolChoiceNone = true
	} else if req.ToolChoiceNone {
		return nil, nil, nil, "", nil, false
	}
	req.Input = combinePromptContent(w.input, req.Input)
	w.disconnected = true
	return w.session, w.rt, w.events, w.retained, &w.responseID, true
}

type comparableNormalizedTool struct {
	Kind         openai.ResponsesToolKind   `json:"kind"`
	Name         string                     `json:"name"`
	Namespace    string                     `json:"namespace,omitempty"`
	Description  string                     `json:"description,omitempty"`
	Parameters   string                     `json:"parameters,omitempty"`
	Format       string                     `json:"format,omitempty"`
	Execution    string                     `json:"execution,omitempty"`
	Strict       *bool                      `json:"strict,omitempty"`
	DeferLoading *bool                      `json:"defer_loading,omitempty"`
	Children     []comparableNormalizedTool `json:"children,omitempty"`
}

func responseToolsEqual(a, b []openai.NormalizedTool) bool {
	ak, aok := responseToolsKey(a)
	bk, bok := responseToolsKey(b)
	return aok && bok && ak == bk
}

func responseToolsKey(tools []openai.NormalizedTool) (string, bool) {
	comparable := comparableTools(tools)
	b, err := json.Marshal(comparable)
	if err != nil {
		return "", false
	}
	return string(b), true
}

func comparableTools(tools []openai.NormalizedTool) []comparableNormalizedTool {
	out := make([]comparableNormalizedTool, 0, len(tools))
	for _, tool := range tools {
		children := comparableTools(tool.Children)
		out = append(out, comparableNormalizedTool{
			Kind:         tool.Kind,
			Name:         tool.Name,
			Namespace:    tool.Namespace,
			Description:  tool.Description,
			Parameters:   canonicalRawJSON(tool.Parameters),
			Format:       canonicalRawJSON(tool.Format),
			Execution:    tool.Execution,
			Strict:       tool.Strict,
			DeferLoading: tool.DeferLoading,
			Children:     children,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func canonicalRawJSON(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return string(trimmed)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(trimmed)
	}
	return string(b)
}

func combinePromptContent(previous openai.PromptContent, current openai.PromptContent) openai.PromptContent {
	if previous.Text != "" {
		if current.Text != "" {
			current.Text = previous.Text + "\n\n" + current.Text
		} else {
			current.Text = previous.Text
		}
	}
	if len(previous.Images) > 0 {
		current.Images = append(append([]openai.ImageInput{}, previous.Images...), current.Images...)
	}
	return current
}

func (g *RealGateway) WarmResponse(ctx context.Context, req ResponseRequest) (*WarmResponseResult, error) {
	if err := g.ValidateModel(ctx, req.Model); err != nil {
		return nil, err
	}
	if req.ResponseID == "" {
		req.ResponseID = openai.NewID("resp_")
	}
	reasoningEffort, err := g.requestReasoningEffort(ctx, req.Model, req.ReasoningEffort, req.DefaultReasoningEffort, req.ResolvedReasoningEffort, req.ReasoningEffortResolved)
	if err != nil {
		return nil, err
	}
	prompt, err := g.resolvePrompt(ctx, req.Model, req.Input, "input")
	if err != nil {
		return nil, err
	}
	rt, err := toolproxy.NewResponseRequestTools(g.broker, req.Tools, req.ToolChoiceNone)
	if err != nil {
		return nil, openai.InvalidRequest(err.Error(), "tools")
	}
	events := make(chan copilot.SessionEvent, 256)
	var session *copilot.Session
	var sessionID string
	var previous *string
	if req.PreviousResponseID != "" {
		record, err := g.store.LoadResponseForContinuation(req.PreviousResponseID)
		if err != nil {
			return nil, openai.PreviousResponseNotFound(req.PreviousResponseID)
		}
		sessionID = record.SDKSessionID
		previous = &req.PreviousResponseID
		session, err = g.resumeSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, true, events)
		if err != nil || session == nil {
			if g.log != nil {
				g.log.Warn("falling back to synthetic warm Responses continuation", "previous_response_id", req.PreviousResponseID, "sdk_session_id", sessionID, "error", err)
			}
			prompt = g.responseContinuationPrompt(record, prompt)
			req.Input.Text = prompt.Text
			sessionID = "resp_sdk_" + uuid.NewString()
			session, err = g.createSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, true, events)
		}
	} else {
		sessionID = "resp_sdk_" + uuid.NewString()
		session, err = g.createSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, true, events)
	}
	if err != nil {
		return nil, openai.Upstream(err.Error())
	}
	if session == nil {
		return nil, openai.Upstream("copilot SDK returned nil session")
	}
	retained := g.fs.SessionRoot(sessionID)
	resp := &openai.Response{ID: req.ResponseID, Object: openai.ObjectResponse, CreatedAt: openai.UnixNow(), Status: "completed", Model: req.Model, Instructions: req.Instructions, Output: []openai.ResponseOutputItem{}, OutputText: "", ParallelToolCalls: true, PreviousResponseID: previous, Store: req.Store, Error: nil, IncompleteDetails: nil}
	record := recordFromResponse(resp, sessionID, retained)
	record.InputText = req.Input.Text
	if err := g.store.SaveResponse(record); err != nil {
		_ = session.Disconnect()
		return nil, openai.Internal(err.Error())
	}
	warm := &WarmResponseSession{responseID: req.ResponseID, sessionID: sessionID, model: req.Model, instructions: req.Instructions, reasoningEffort: reasoningEffort, tools: append([]openai.NormalizedTool{}, req.Tools...), toolChoiceNone: req.ToolChoiceNone, input: req.Input, previous: previous, store: req.Store, retained: retained, session: session, rt: rt, events: events}
	return &WarmResponseResult{Response: resp, WarmSession: warm}, nil
}
