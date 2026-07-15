package copilotgw

import (
	"context"
	"sync"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
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
	input           resolvedPrompt
	imageBudget     *imageRequestBudget
	pinReleases     []func()
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
	pinReleases := w.pinReleases
	w.imageBudget = nil
	w.pinReleases = nil
	w.mu.Unlock()
	releaseAll(pinReleases)
	if session != nil {
		_ = session.Disconnect()
	}
}

type warmResponseUse struct {
	session     *copilot.Session
	tools       *toolproxy.RequestTools
	events      chan copilot.SessionEvent
	retained    string
	previous    *string
	prompt      resolvedPrompt
	imageBudget *imageRequestBudget
	pinReleases []func()
}

func (w *WarmResponseSession) use(req *ResponseRequest) (warmResponseUse, bool) {
	if w == nil || req == nil || req.PreviousResponseID == "" {
		return warmResponseUse{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.disconnected || req.PreviousResponseID != w.responseID || req.Model != w.model {
		return warmResponseUse{}, false
	}
	if req.Instructions == "" {
		req.Instructions = w.instructions
	} else if req.Instructions != w.instructions {
		return warmResponseUse{}, false
	}
	requestReasoningEffort := cleanReasoningEffort(req.ReasoningEffort)
	if requestReasoningEffort == "" {
		req.ReasoningEffort = w.reasoningEffort
	} else if requestReasoningEffort != w.reasoningEffort {
		return warmResponseUse{}, false
	}
	if !req.ToolsSet && len(req.Tools) == 0 {
		req.Tools = append([]openai.NormalizedTool{}, w.tools...)
	} else if !responseToolsEqual(req.Tools, w.tools) {
		return warmResponseUse{}, false
	}
	if w.toolChoiceNone {
		req.ToolChoiceNone = true
	} else if req.ToolChoiceNone {
		return warmResponseUse{}, false
	}
	used := warmResponseUse{
		session: w.session, tools: w.rt, events: w.events, retained: w.retained,
		previous: &w.responseID, prompt: w.input, imageBudget: w.imageBudget,
		pinReleases: w.pinReleases,
	}
	w.imageBudget = nil
	w.pinReleases = nil
	w.disconnected = true
	return used, true
}

func releaseAll(releases []func()) {
	for _, release := range releases {
		if release != nil {
			release()
		}
	}
}

func responseToolsEqual(a, b []openai.NormalizedTool) bool {
	ac, err := openai.NewToolCatalog(a)
	if err != nil {
		return false
	}
	bc, err := openai.NewToolCatalog(b)
	if err != nil {
		return false
	}
	return ac.Key() == bc.Key()
}

func (g *RealGateway) WarmResponse(ctx context.Context, req ResponseRequest) (*WarmResponseResult, error) {
	if len(req.ToolOutputs) > 0 {
		return nil, openai.InvalidRequest("generate:false with tool-output continuations is not supported", "input")
	}
	if err := g.ValidateModel(ctx, req.Model); err != nil {
		return nil, err
	}
	if req.ResponseID == "" {
		req.ResponseID = openai.NewID("resp_")
	}
	incrementalInput := req.Input.Text
	reasoningEffort, err := g.requestReasoningEffort(ctx, req.Model, req.ReasoningEffort, req.DefaultReasoningEffort, req.ResolvedReasoningEffort, req.ReasoningEffortResolved)
	if err != nil {
		return nil, err
	}
	imageBudget := newImageRequestBudget()
	prompt, err := g.resolvePromptWithImageBudget(ctx, req.Model, req.Input, "input", imageBudget)
	if err != nil {
		return nil, err
	}
	var previousRecord *sessionstore.ResponseRecord
	var previousPins []func()
	defer func() { releaseAll(previousPins) }()
	if req.PreviousResponseID != "" {
		previousPins = append(previousPins, g.store.PinResponse(req.PreviousResponseID))
		record, err := g.store.LoadResponseForContinuation(req.PreviousResponseID)
		if err != nil {
			return nil, openai.PreviousResponseNotFound(req.PreviousResponseID)
		}
		previousPins = append(previousPins, g.store.PinSession(record.SDKSessionID))
		previousRecord = &record
	}
	catalog, err := responseCatalogForRequest(req, previousRecord)
	if err != nil {
		return nil, err
	}
	rt, err := toolproxy.NewResponseRequestTools(g.broker, catalog.Flatten(), req.ToolChoiceNone)
	if err != nil {
		return nil, openai.InvalidRequest(err.Error(), "tools")
	}
	events := make(chan copilot.SessionEvent, 256)
	var session *copilot.Session
	var sessionID string
	var previous *string
	var earlySessionPin func()
	keepSessionPin := false
	defer func() {
		if !keepSessionPin && earlySessionPin != nil {
			earlySessionPin()
		}
	}()
	if previousRecord != nil {
		sessionID = previousRecord.SDKSessionID
		previous = &req.PreviousResponseID
		if !req.ForceSynthetic {
			session, err = g.resumeSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, true, events)
		}
		if req.ForceSynthetic || err != nil || session == nil {
			if g.log != nil && !req.ForceSynthetic {
				g.log.Warn("falling back to synthetic warm Responses continuation", "previous_response_id", req.PreviousResponseID, "sdk_session_id", sessionID, "error", err)
			}
			prompt = g.responseContinuationPrompt(*previousRecord, prompt)
			req.Input.Text = prompt.Text
			sessionID = "resp_sdk_" + uuid.NewString()
			earlySessionPin = g.store.PinSession(sessionID)
			session, err = g.createSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, true, events)
		}
	} else {
		sessionID = "resp_sdk_" + uuid.NewString()
		earlySessionPin = g.store.PinSession(sessionID)
		session, err = g.createSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, true, events)
	}
	if err != nil {
		return nil, openai.Upstream(err.Error())
	}
	if session == nil {
		return nil, openai.Upstream("copilot SDK returned nil session")
	}
	retained := g.fs.SessionRoot(sessionID)
	if earlySessionPin == nil {
		earlySessionPin = g.store.PinSession(sessionID)
	}
	pinReleases := []func(){earlySessionPin, g.store.PinResponse(req.ResponseID)}
	keepSessionPin = true
	keepPins := false
	defer func() {
		if !keepPins {
			releaseAll(pinReleases)
		}
	}()
	resp := &openai.Response{ID: req.ResponseID, Object: openai.ObjectResponse, CreatedAt: openai.UnixNow(), Status: "completed", Model: req.Model, Instructions: req.Instructions, Output: []openai.ResponseOutputItem{}, OutputText: "", ParallelToolCalls: true, PreviousResponseID: previous, Store: req.Store, Error: nil, IncompleteDetails: nil}
	record := recordFromResponse(resp, sessionID, retained)
	record.InputText = incrementalInput
	record.InstalledToolCatalog = storeToolCatalog(catalog.StoredDTO())
	if err := g.store.SaveResponse(record); err != nil {
		_ = session.Disconnect()
		return nil, openai.Internal("failed to persist response")
	}
	warm := &WarmResponseSession{responseID: req.ResponseID, sessionID: sessionID, model: req.Model, instructions: req.Instructions, reasoningEffort: reasoningEffort, tools: catalog.Flatten(), toolChoiceNone: req.ToolChoiceNone, input: prompt, imageBudget: imageBudget, pinReleases: pinReleases, previous: previous, store: req.Store, retained: retained, session: session, rt: rt, events: events}
	keepPins = true
	return &WarmResponseResult{Response: resp, WarmSession: warm}, nil
}
