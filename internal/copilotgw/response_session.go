package copilotgw

import (
	"context"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/google/uuid"
)

type preparedResponseTurn struct {
	session     *copilot.Session
	sessionID   string
	previous    *string
	rt          *toolproxy.RequestTools
	events      chan copilot.SessionEvent
	prompt      resolvedPrompt
	catalog     openai.ToolCatalog
	retained    string
	imageBudget *imageRequestBudget
	pinReleases []func()
}

func (g *RealGateway) prepareResponseTurn(ctx context.Context, req *ResponseRequest, streaming bool) (*preparedResponseTurn, error) {
	reasoningEffort, err := g.requestReasoningEffort(ctx, req.Model, req.ReasoningEffort, req.DefaultReasoningEffort, req.ResolvedReasoningEffort, req.ReasoningEffortResolved)
	if err != nil {
		return nil, err
	}

	prepared := &preparedResponseTurn{events: make(chan copilot.SessionEvent, 256), imageBudget: newImageRequestBudget()}
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
			prepared.sessionID = warmUse.session.SessionID
			prepared.catalog, err = openai.NewToolCatalog(req.Tools)
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
				return nil, openai.PreviousResponseNotFound(req.PreviousResponseID)
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
			return nil, openai.InvalidRequest(err.Error(), "tools")
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
				prepared.prompt = g.responseContinuationPrompt(*previousRecord, prepared.prompt)
				prepared.sessionID = "resp_sdk_" + uuid.NewString()
				prepared.pinReleases = append(prepared.pinReleases, g.store.PinSession(prepared.sessionID))
				prepared.session, err = g.createSession(ctx, prepared.sessionID, req.Model, req.Instructions, reasoningEffort, prepared.rt, streaming, prepared.events)
			}
		} else {
			prepared.sessionID = "resp_sdk_" + uuid.NewString()
			prepared.pinReleases = append(prepared.pinReleases, g.store.PinSession(prepared.sessionID))
			prepared.session, err = g.createSession(ctx, prepared.sessionID, req.Model, req.Instructions, reasoningEffort, prepared.rt, streaming, prepared.events)
		}
		if err != nil {
			return nil, openai.Upstream(err.Error())
		}
	}

	if prepared.session == nil {
		return nil, openai.Upstream("copilot SDK returned nil session")
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
