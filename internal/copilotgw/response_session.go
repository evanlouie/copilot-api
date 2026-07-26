package copilotgw

import (
	"context"
	"slices"
	"strings"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/google/uuid"
)

type preparedResponseTurn struct {
	session     copilotSession
	sessionID   string
	previous    *string
	rt          *toolproxy.RequestTools
	events      *sessionEventSink
	prompt      resolvedPrompt
	catalog     toolcatalog.ToolCatalog
	retained    string
	imageBudget *imageRequestBudget
	pinReleases []func()
	// pendingInput names the warm records whose buffered input this turn's prompt
	// carries. They stay pending until the send that delivers them succeeds; see
	// markPendingInputDelivered.
	pendingInput []string
}

func (g *RealGateway) prepareResponseTurn(ctx context.Context, req *ResponseRequest, streaming bool) (*preparedResponseTurn, error) {
	reasoningEffort, err := g.requestReasoningEffort(ctx, req.Model, req.ReasoningEffort, req.DefaultReasoningEffort, req.ResolvedReasoningEffort, req.ReasoningEffortResolved)
	if err != nil {
		return nil, err
	}

	prepared := &preparedResponseTurn{events: newSessionEventSink(g.log), imageBudget: newImageRequestBudget()}
	keepPins := false
	defer func() {
		if !keepPins {
			releaseAll(prepared.pinReleases)
		}
	}()
	promptResolved := false
	if streaming {
		if warmUse, ok := req.WarmSession.use(req); ok {
			prepared.imageBudget = warmUse.imageBudget
			prepared.pinReleases = warmUse.pinReleases
			if prepared.imageBudget == nil {
				prepared.imageBudget = newImageRequestBudget()
			}
			currentPrompt, resolveErr := g.resolvePromptWithImageBudget(ctx, req.Model, req.Input, "input", prepared.imageBudget)
			if resolveErr != nil {
				_ = warmUse.session.Disconnect()
				return nil, resolveErr
			}
			prepared.prompt = combineResolvedPrompts(warmUse.prompt, currentPrompt)
			promptResolved = true
			prepared.pendingInput = warmUse.pendingInput
			prepared.session = warmUse.session
			prepared.rt = warmUse.tools
			prepared.events = warmUse.events
			prepared.retained = warmUse.retained
			prepared.previous = warmUse.previous
			prepared.sessionID = warmUse.session.ID()
			prepared.catalog, err = toolcatalog.NewToolCatalog(req.Tools)
			if err != nil {
				_ = prepared.session.Disconnect()
				return nil, err
			}
		} else if req.WarmSession != nil && req.WarmSession.ResponseID() == req.PreviousResponseID {
			req.WarmSession.Disconnect()
		}
	}

	if prepared.session == nil {
		prepared.prompt, err = g.resolvePromptWithImageBudget(ctx, req.Model, req.Input, "input", prepared.imageBudget)
		if err != nil {
			return nil, err
		}
		promptResolved = true
		var previousRecord *sessionstore.ResponseRecord
		if req.PreviousResponseID != "" {
			prepared.pinReleases = append(prepared.pinReleases, g.store.PinResponse(req.PreviousResponseID))
			record, loadErr := g.store.LoadResponseForContinuation(req.PreviousResponseID)
			if loadErr != nil {
				return nil, apierr.PreviousResponseNotFound(req.PreviousResponseID)
			}
			prepared.pinReleases = append(prepared.pinReleases, g.store.PinSession(record.SDKSessionID))
			previousRecord = &record
		}
		prepared.catalog, err = responseCatalogForRequest(*req, previousRecord)
		if err != nil {
			return nil, err
		}
		prepared.rt, err = toolproxy.NewResponseRequestTools(g.broker, prepared.catalog.Flatten(), req.ToolChoiceNone)
		if err != nil {
			return nil, apierr.InvalidRequest(err.Error(), "tools")
		}
		g.logUnenforceableStrict(prepared.rt, "responses")
		if previousRecord != nil {
			prepared.sessionID = previousRecord.SDKSessionID
			prepared.previous = &req.PreviousResponseID
			if !req.ForceSynthetic {
				prepared.session, err = g.resumeSession(ctx, prepared.sessionID, req.Model, req.Instructions, reasoningEffort, prepared.rt, streaming, prepared.events)
			}
			if req.ForceSynthetic || err != nil || prepared.session == nil {
				if !req.ForceSynthetic {
					g.log.Warn("falling back to synthetic Responses continuation", "previous_response_id", req.PreviousResponseID, "sdk_session_id", prepared.sessionID, "streaming", streaming, "error", err)
				}
				prepared.sessionID = "resp_sdk_" + uuid.NewString()
				prepared.pinReleases = append(prepared.pinReleases, g.store.PinSession(prepared.sessionID))
				if hydrateErr := g.hydrateResponseContinuation(prepared.sessionID, req.Model, *previousRecord); hydrateErr != nil {
					if g.log != nil {
						g.log.Warn("falling back to a prose transcript for a cold Responses continuation", "previous_response_id", req.PreviousResponseID, "error", hydrateErr)
					}
					prepared.prompt = g.responseContinuationPrompt(*previousRecord, prepared.prompt)
					prepared.session, err = g.createSession(ctx, prepared.sessionID, req.Model, req.Instructions, reasoningEffort, prepared.rt, streaming, prepared.events)
				} else {
					// The chain now lives in the synthetic session's events, so the
					// prompt carries only this request's own input.
					prepared.prompt = responseContinuationFollowUp(prepared.prompt)
					prepared.session, err = g.resumeSession(ctx, prepared.sessionID, req.Model, req.Instructions, reasoningEffort, prepared.rt, streaming, prepared.events)
				}
			} else {
				// The resumed session is the one the previous record names, so it holds
				// everything the chain's turns actually sent - and nothing a warm
				// (generate:false) response in that chain only buffered. Replaying the
				// buffered run here is what makes warming survive a dropped WebSocket
				// or a restart.
				pending := g.pendingInputForSession(*previousRecord)
				prepared.pendingInput = pending.responseIDs
				prepared.prompt = combineResolvedPrompts(pending.prompt, prepared.prompt)
			}
		} else {
			prepared.sessionID = "resp_sdk_" + uuid.NewString()
			prepared.pinReleases = append(prepared.pinReleases, g.store.PinSession(prepared.sessionID))
			prepared.session, err = g.createSession(ctx, prepared.sessionID, req.Model, req.Instructions, reasoningEffort, prepared.rt, streaming, prepared.events)
		}
		if err != nil {
			return nil, classifyUpstreamError(err)
		}
	}

	if prepared.session == nil {
		return nil, apierr.Upstream("copilot SDK returned nil session")
	}
	if !promptResolved {
		prepared.prompt, err = g.resolvePromptWithImageBudget(ctx, req.Model, req.Input, "input", prepared.imageBudget)
		if err != nil {
			_ = prepared.session.Disconnect()
			return nil, err
		}
	}
	if prepared.retained == "" {
		prepared.retained = g.fs.SessionRoot(prepared.sessionID)
	}
	keepPins = true
	return prepared, nil
}

// pendingSessionInput is everything a run of warm (generate:false) responses
// buffered for one SDK session and never sent.
type pendingSessionInput struct {
	// responseIDs names the records holding that input, oldest first. They are
	// what markPendingInputDelivered settles once a send has put prompt into the
	// session.
	responseIDs []string
	prompt      resolvedPrompt
}

// pendingInputForSession collects the input the record chain ending at previous
// buffered for previous.SDKSessionID without ever sending it.
//
// Warming is chainable: generate:false itself accepts previous_response_id and
// resumes that response's SDK session, so a client can prime ALPHA, prime BRAVO
// on top of it, and have neither reach the session. Reading only the immediate
// previous record recovers BRAVO and silently drops ALPHA, even though the
// client was told both had been accepted.
//
// The walk stops at the first record that has already delivered its input or
// that names a different SDK session. A record on another session is not this
// session's business: the synthetic-continuation fallback opens a fresh session
// and replays such chains into it as history, so their input is already there.
//
// Only text survives. A warm session's in-memory resolvedPrompt also carries
// fetched image attachments, and those are held in memory alone; a client that
// warms with images and then loses its WebSocket keeps the text and re-fetches
// nothing.
func (g *RealGateway) pendingInputForSession(previous sessionstore.ResponseRecord) pendingSessionInput {
	var pending pendingSessionInput
	var texts []string
	seen := map[string]struct{}{}
	record := previous
	for len(seen) < responseContinuationChainLimit {
		if _, ok := seen[record.ID]; ok {
			break
		}
		seen[record.ID] = struct{}{}
		if !record.InputPending || record.SDKSessionID != previous.SDKSessionID {
			break
		}
		// A pending record with no text is still settled: there is nothing to
		// replay, and leaving the claim standing only invites re-examining it.
		pending.responseIDs = append(pending.responseIDs, record.ID)
		if strings.TrimSpace(record.InputText) != "" {
			texts = append(texts, record.InputText)
		}
		if record.PreviousResponseID == "" {
			break
		}
		earlier, err := g.store.LoadResponseForContinuation(record.PreviousResponseID)
		if err != nil || earlier.Deleted {
			break
		}
		record = earlier
	}
	slices.Reverse(pending.responseIDs)
	slices.Reverse(texts)
	pending.prompt = resolvedPrompt{Text: strings.Join(texts, "\n\n")}
	return pending
}

// markPendingInputDelivered retires the buffered-input claims a send has just
// satisfied. It runs after Send returns and never before, because Send returning
// is the moment that text is demonstrably inside the SDK session.
//
// That ordering makes delivery at-least-once rather than exactly-once: a process
// that dies between Send returning and these writes landing - or a disk error on
// them, which is logged rather than propagated because the input is already in
// the session - leaves the claims standing, so the next resume of that session
// replays input the conversation already has. On the streaming path the writes
// also race the turn's own completion, which widens that window from a crash to
// "another request resumed the same warm response id in the last few
// milliseconds". The opposite order - clearing first - would turn every failed
// send into silently dropped input the client was told had been accepted. A
// repeated turn is recoverable; a lost one is not.
func (g *RealGateway) markPendingInputDelivered(responseIDs []string) {
	for _, id := range responseIDs {
		if err := g.store.ClearInputPending(id); err != nil && g.log != nil {
			g.log.Warn("failed to record warmed input as delivered; a later resume may repeat it", "response_id", id, "error", err)
		}
	}
}

func combineResolvedPrompts(previous, current resolvedPrompt) resolvedPrompt {
	if previous.Text != "" {
		if current.Text != "" {
			current.Text = previous.Text + "\n\n" + current.Text
		} else {
			current.Text = previous.Text
		}
	}
	if len(previous.Attachments) > 0 {
		current.Attachments = append(append([]copilot.Attachment{}, previous.Attachments...), current.Attachments...)
	}
	return current
}
