package copilotgw

import (
	"context"
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
				// everything that record's turn actually sent - and nothing it only
				// buffered. Replaying pending input here is what makes a warm
				// (generate:false) response survive a dropped WebSocket or a restart.
				prepared.prompt = combineResolvedPrompts(pendingInputPrompt(*previousRecord), prepared.prompt)
			}
		} else {
			prepared.sessionID = "resp_sdk_" + uuid.NewString()
			prepared.pinReleases = append(prepared.pinReleases, g.store.PinSession(prepared.sessionID))
			prepared.session, err = g.createSession(ctx, prepared.sessionID, req.Model, req.Instructions, reasoningEffort, prepared.rt, streaming, prepared.events)
		}
		if err != nil {
			return nil, apierr.Upstream(err.Error())
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

// pendingInputPrompt returns the input a record buffered without ever sending it
// to its SDK session, which only a warm (generate:false) response produces.
//
// Only text survives. The warm session's resolvedPrompt also carries fetched
// image attachments, and those are held in memory alone; a client that warms
// with images and then loses its WebSocket keeps the text and re-fetches nothing.
//
// The flag is never cleared once consumed. It records a durable fact about the
// record's own SDK session - "this input was buffered, not sent as its own turn"
// - and clearing it would mean writing the store back before the send that
// consumes it has succeeded, which would drop the input on a failed turn. The
// only way to consume it twice is for a client to branch from the same warm
// response id more than once, and branching already resumes the single shared
// SDK session rather than forking it, so that path is approximate either way.
// Repeating a warmed prompt on a re-branch is a much smaller wrong than silently
// dropping input the client was told had been accepted.
func pendingInputPrompt(record sessionstore.ResponseRecord) resolvedPrompt {
	if !record.InputPending || strings.TrimSpace(record.InputText) == "" {
		return resolvedPrompt{}
	}
	return resolvedPrompt{Text: record.InputText}
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
