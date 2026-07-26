package copilotgw

import (
	"context"
	"sync"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	"github.com/google/uuid"
)

type WarmResponseSession struct {
	mu              sync.Mutex
	responseID      string
	sessionID       string
	model           string
	instructions    string
	reasoningEffort string
	tools           []toolcatalog.NormalizedTool
	toolChoice      openai.ToolChoice
	input           resolvedPrompt
	// pendingInput names every record whose buffered input this session's primed
	// prompt carries, including this session's own response. Whoever generates
	// from the session settles all of them.
	pendingInput []string
	imageBudget  *imageRequestBudget
	pinReleases  []func()
	previous     *string
	store        bool
	retained     string
	session      copilotSession
	rt           *toolproxy.RequestTools
	events       *sessionEventSink
	disconnected bool
	// registry is the gateway registry that owns this session's shutdown. Both
	// paths that hand ownership away - Disconnect and use - deregister from it, so
	// gateway Stop never sees a session it no longer owns.
	registry *warmSessionRegistry
}

// attachRegistry records which gateway registry owns this session's shutdown.
func (w *WarmResponseSession) attachRegistry(registry *warmSessionRegistry) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.registry = registry
	w.mu.Unlock()
}

// Disconnected reports whether this session's SDK session and retention pins
// have already been handed away or torn down.
//
// It is exported because the HTTP layer owns a warm session's lifetime between
// requests on a WebSocket connection, and its tests assert that a session
// offered to connection state that is already closed is disconnected rather
// than stored - which is not observable from outside this package otherwise.
func (w *WarmResponseSession) Disconnected() bool {
	if w == nil {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.disconnected
}

func (w *WarmResponseSession) ResponseID() string {
	if w == nil {
		return ""
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.responseID
}

// Disconnect tears the warm session down: it releases the retention pins the
// session owns and drops its SDK session. It is idempotent, which is what makes
// a client disconnect racing gateway shutdown safe - whichever wins flips
// disconnected under the mutex and the loser returns without touching anything.
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
	registry := w.registry
	w.imageBudget = nil
	w.pinReleases = nil
	w.registry = nil
	w.mu.Unlock()
	// Ownership ends here, so the gateway must stop counting this session before
	// anything else. remove is a no-op when Stop already snapshotted it.
	registry.remove(w)
	releaseAll(pinReleases)
	if session != nil {
		_ = session.Disconnect()
	}
}

type warmResponseUse struct {
	session      copilotSession
	tools        *toolproxy.RequestTools
	events       *sessionEventSink
	retained     string
	previous     *string
	prompt       resolvedPrompt
	pendingInput []string
	imageBudget  *imageRequestBudget
	pinReleases  []func()
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
		req.Tools = append([]toolcatalog.NormalizedTool{}, w.tools...)
	} else if !responseToolsEqual(req.Tools, w.tools) {
		return warmResponseUse{}, false
	}
	// The warm session's SDK session was configured with the catalog its own
	// tool_choice implied, and an SDK session's AvailableTools cannot be changed
	// after the fact. So reuse is only sound when this request asks for the same
	// catalog: an unset tool_choice inherits the warm session's, exactly as
	// instructions and reasoning effort do above, and anything else has to agree
	// on scope rather than on spelling, because an omitted tool_choice and
	// "auto" name the same catalog. A mismatch is not an error - the caller
	// falls back to resuming the chain from the store, which rebuilds the
	// catalog this request asked for.
	if req.ToolChoice.Kind == "" {
		req.ToolChoice = w.toolChoice
	} else if !req.ToolChoice.Scope().Equal(w.toolChoice.Scope()) {
		return warmResponseUse{}, false
	}
	// The SDK session, its pins and its events are about to become the caller's
	// turn runner's. Ask the gateway registry to give up ownership first: it
	// refuses once Stop has closed it, and refusing here is what keeps a session
	// from ending up owned by neither registry.
	if !w.registry.release(w) {
		return warmResponseUse{}, false
	}
	used := warmResponseUse{
		session: w.session, tools: w.rt, events: w.events, retained: w.retained,
		previous: &w.responseID, prompt: w.input, imageBudget: w.imageBudget,
		pendingInput: w.pendingInput, pinReleases: w.pinReleases,
	}
	w.imageBudget = nil
	w.pendingInput = nil
	w.pinReleases = nil
	w.registry = nil
	// Marking the session disconnected keeps a racing gateway Stop from
	// disconnecting it a second time; release above already keeps Stop from
	// seeing it at all.
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

func responseToolsEqual(a, b []toolcatalog.NormalizedTool) bool {
	ac, err := toolcatalog.NewToolCatalog(a)
	if err != nil {
		return false
	}
	bc, err := toolcatalog.NewToolCatalog(b)
	if err != nil {
		return false
	}
	return ac.Key() == bc.Key()
}

func (g *RealGateway) WarmResponse(ctx context.Context, req ResponseRequest) (*WarmResponseResult, error) {
	if len(req.ToolOutputs) > 0 {
		return nil, apierr.InvalidRequest("generate:false with tool-output continuations is not supported", "input")
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
			return nil, apierr.PreviousResponseNotFound(req.PreviousResponseID)
		}
		previousPins = append(previousPins, g.store.PinSession(record.SDKSessionID))
		previousRecord = &record
	}
	catalog, err := responseCatalogForRequest(req, previousRecord)
	if err != nil {
		return nil, err
	}
	rt, err := toolproxy.NewResponseRequestTools(g.broker, catalog.Flatten(), req.ToolChoice.Scope())
	if err != nil {
		return nil, requestToolsError(err)
	}
	g.logUnenforceableStrict(rt, "responses.warm")
	events := newSessionEventSink(g.log)
	var session copilotSession
	var sessionID string
	var previous *string
	// pendingInput carries the buffered input of any earlier warm responses in
	// this chain, which this warm session now speaks for.
	var pendingInput []string
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
			sessionID = "resp_sdk_" + uuid.NewString()
			earlySessionPin = g.store.PinSession(sessionID)
			if hydrateErr := g.hydrateResponseContinuation(sessionID, req.Model, *previousRecord); hydrateErr != nil {
				if g.log != nil {
					g.log.Warn("falling back to a prose transcript for a cold warm Responses continuation", "previous_response_id", req.PreviousResponseID, "error", hydrateErr)
				}
				prompt = g.responseContinuationPrompt(*previousRecord, prompt)
				req.Input.Text = prompt.Text
				session, err = g.createSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, true, events)
			} else {
				// The chain replayed as session events, so the warm session's primed
				// prompt stays exactly what the client sent.
				session, err = g.resumeSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, true, events)
			}
		} else {
			// Warming on top of warming: the session just resumed still has not
			// received anything an earlier warm response in this chain buffered, and
			// this request sends nothing either. Fold that run into the prompt this
			// session primes, so the turn that finally generates delivers the whole
			// chain instead of only the newest link. Recording it durably is not
			// enough on its own: the in-memory warm session is what a client on a
			// live WebSocket actually generates from.
			pending := g.pendingInputForSession(*previousRecord)
			pendingInput = pending.responseIDs
			prompt = combineResolvedPrompts(pending.prompt, prompt)
		}
	} else {
		sessionID = "resp_sdk_" + uuid.NewString()
		earlySessionPin = g.store.PinSession(sessionID)
		session, err = g.createSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, true, events)
	}
	if err != nil {
		return nil, classifyUpstreamError(err)
	}
	if session == nil {
		return nil, apierr.Upstream("copilot SDK returned nil session")
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
	resp := &openai.Response{ID: req.ResponseID, Object: openai.ObjectResponse, CreatedAt: warmResponseCreatedAt(req), Status: "completed", Model: req.Model, Instructions: req.Instructions, Output: []openai.ResponseOutputItem{}, OutputText: "", ParallelToolCalls: true, PreviousResponseID: previous, Store: req.Store, Metadata: req.Metadata, Error: nil, IncompleteDetails: nil}
	record := recordFromResponse(resp, sessionID, retained)
	record.InputText = incrementalInput
	// The whole point of generate:false is that this input is primed, not sent:
	// it lives in warm.input and reaches the SDK only when a later request
	// generates. Recording that it is still undelivered is what lets a resume
	// replay it after the client's WebSocket drops or this process restarts. Only
	// this request's own text is recorded here - any earlier warm input in the
	// chain is still claimed by its own record, so folding it in above is an
	// in-memory convenience and not a second durable copy.
	record.InputPending = true
	record.InstalledToolCatalog = catalog.StoredDTO()
	if err := g.store.SaveResponse(record); err != nil {
		_ = session.Disconnect()
		return nil, apierr.Internal("failed to persist response")
	}
	warm := &WarmResponseSession{responseID: req.ResponseID, sessionID: sessionID, model: req.Model, instructions: req.Instructions, reasoningEffort: reasoningEffort, tools: catalog.Flatten(), toolChoice: req.ToolChoice, input: prompt, pendingInput: append(pendingInput, req.ResponseID), imageBudget: imageBudget, pinReleases: pinReleases, previous: previous, store: req.Store, retained: retained, session: session, rt: rt, events: events}
	// The session now owns the pins, so the deferred release must not fire; from
	// here on teardown goes through warm.Disconnect.
	keepPins = true
	if !g.trackWarmSession(warm) {
		// Stop already snapshotted the registry, so nothing would ever drain this
		// session. Tear it down here instead of handing a leak to the client.
		warm.Disconnect()
		// A shutdown is this service declining the request, not a dependency
		// failing: 503, and retrying once the process is back is the right move.
		return nil, apierr.Unavailable("gateway is shutting down")
	}
	return &WarmResponseResult{Response: resp, WarmSession: warm}, nil
}
