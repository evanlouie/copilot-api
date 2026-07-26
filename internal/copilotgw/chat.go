package copilotgw

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/hydration"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionfs"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/google/uuid"
)

type preparedChatTurn struct {
	sessionID  string
	retained   string
	final      resolvedPrompt
	rt         *toolproxy.RequestTools
	events     *sessionEventSink
	session    copilotSession
	pinRelease func()
}

func (g *RealGateway) prepareChatTurn(ctx context.Context, req ChatRequest, streaming bool) (*preparedChatTurn, error) {
	if err := g.ValidateModel(ctx, req.Model); err != nil {
		return nil, err
	}
	reasoningEffort, err := g.requestReasoningEffort(ctx, req.Model, req.ReasoningEffort, req.DefaultReasoningEffort, req.ResolvedReasoningEffort, req.ReasoningEffortResolved)
	if err != nil {
		return nil, err
	}
	finalPrompt, err := req.FinalUser.Prompt()
	if err != nil {
		return nil, apierr.InvalidRequest(err.Error(), "messages")
	}
	imageBudget := newImageRequestBudget()
	final, err := g.resolvePromptWithImageBudget(ctx, req.Model, finalPrompt, "messages", imageBudget)
	if err != nil {
		return nil, err
	}
	history, err := g.resolveChatHistoryWithImageBudget(ctx, req.Model, req.History, imageBudget)
	if err != nil {
		return nil, err
	}
	sessionID := "chat_" + uuid.NewString()
	pinRelease := g.store.PinSession(sessionID)
	keepPin := false
	defer func() {
		if !keepPin {
			pinRelease()
		}
	}()
	h, err := hydration.BuildChatHistoryJSONL(history, hydration.Options{SessionID: sessionID, Model: req.Model})
	if err != nil {
		return nil, apierr.InvalidRequest("failed to hydrate chat history: "+err.Error(), "messages")
	}
	retained, err := sessionfs.WriteEvents(g.cfg.DataDir, sessionID, h.JSONL)
	if err != nil {
		return nil, apierr.Internal("failed to write synthetic session state")
	}
	rt, err := toolproxy.NewRequestTools(g.broker, req.Tools, req.ToolChoice.Scope())
	if err != nil {
		return nil, requestToolsError(err)
	}
	if err := g.reportUnenforceableStrict(rt, "chat"); err != nil {
		return nil, err
	}
	events := newSessionEventSink(g.log)
	session, err := g.resumeSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, streaming, events)
	if err != nil {
		return nil, classifyUpstreamError(err)
	}
	if session == nil {
		return nil, apierr.Upstream("copilot SDK returned nil session")
	}
	keepPin = true
	return &preparedChatTurn{sessionID: sessionID, retained: retained, final: final, rt: rt, events: events, session: session, pinRelease: pinRelease}, nil
}

func (g *RealGateway) Chat(ctx context.Context, req ChatRequest) (*TurnResult, error) {
	prepared, err := g.prepareChatTurn(ctx, req, false)
	if err != nil {
		return nil, err
	}
	releaseSession := g.store.PinSession(prepared.sessionID)
	defer releaseSession()
	defer prepared.pinRelease()
	runner, err := g.newTurnRunner(ctx, req.OpenAIID, req.Model, prepared.session, prepared.rt, prepared.events, prepared.retained, "chat", "")
	if err != nil {
		return nil, err
	}
	runner.watchContext(ctx)
	if _, err := prepared.session.Send(ctx, copilot.MessageOptions{Prompt: prepared.final.Text, Attachments: prepared.final.Attachments}); err != nil {
		runner.failSend(prepared.events, err)
		_, _ = runner.waitInitial(ctx)
		return nil, classifyUpstreamError(err)
	}
	result, err := runner.waitInitial(ctx)
	if err != nil {
		return nil, err
	}
	if result.PendingBatchID != "" {
		g.rememberRunner(result.PendingBatchID, runner)
	}
	g.saveChatSessionMetadata(prepared.sessionID, prepared.retained, req.Model, result)
	return result, nil
}

func (g *RealGateway) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	prepared, err := g.prepareChatTurn(ctx, req, true)
	if err != nil {
		return nil, err
	}
	ch := make(chan StreamEvent, 32)
	defer prepared.pinRelease()
	runner, err := g.newTurnRunner(ctx, req.OpenAIID, req.Model, prepared.session, prepared.rt, prepared.events, prepared.retained, "chat", "")
	if err != nil {
		close(ch)
		return nil, err
	}
	runner.watchContext(ctx)
	runner.enableChatStream(ch, ctx.Done())
	runner.setOnResult(func(result *TurnResult) error {
		if result.PendingBatchID != "" {
			g.rememberRunner(result.PendingBatchID, runner)
		}
		g.saveChatSessionMetadata(prepared.sessionID, prepared.retained, req.Model, result)
		return nil
	})
	go runner.discardInitial()
	go func() {
		runner.debug(g, "copilot send started", "prompt_bytes", len(prepared.final.Text), "attachment_count", len(prepared.final.Attachments))
		if _, err := prepared.session.Send(ctx, copilot.MessageOptions{Prompt: prepared.final.Text, Attachments: prepared.final.Attachments}); err != nil {
			runner.debug(g, "copilot send failed", "error", err.Error())
			runner.failSend(prepared.events, err)
			return
		}
		runner.debug(g, "copilot send returned")
	}()
	return ch, nil
}

func (g *RealGateway) saveChatSessionMetadata(sessionID, retained, model string, result *TurnResult) {
	now := time.Now().UTC()
	err := g.store.SaveSessionMetadata(sessionID, sessionstore.SessionMetadata{
		ID: sessionID, Kind: "chat", OpenAIID: result.ID, SDKSessionID: sessionID,
		Model: model, CreatedAt: now, UpdatedAt: now, RetainedPath: retained,
		FinishReason: result.FinishReason, PendingBatchID: result.PendingBatchID,
	})
	if err != nil {
		g.log.Warn("failed to save chat session metadata", "session_id", sessionID, "error", err)
	}
}

func (g *RealGateway) resolveChatHistoryWithImageBudget(ctx context.Context, model string, messages []openai.ChatMessage, imageBudget *imageRequestBudget) ([]hydration.Message, error) {
	out := make([]hydration.Message, 0, len(messages))
	for i, msg := range messages {
		switch msg.Role {
		case "user":
			prompt, err := msg.Prompt()
			if err != nil {
				return nil, apierr.InvalidRequest(err.Error(), fmt.Sprintf("messages.%d.content", i))
			}
			resolved, err := g.resolvePromptWithImageBudget(ctx, model, prompt, fmt.Sprintf("messages.%d.content", i), imageBudget)
			if err != nil {
				return nil, err
			}
			out = append(out, hydration.Message{Role: msg.Role, Content: resolved.Text, Attachments: resolved.Attachments})
		case "assistant", "tool":
			text, err := msg.Text()
			if err != nil {
				return nil, apierr.InvalidRequest(err.Error(), fmt.Sprintf("messages.%d.content", i))
			}
			out = append(out, hydration.Message{Role: msg.Role, Content: text, Reasoning: msg.InboundReasoning(), ToolCallID: msg.ToolCallID, ToolCalls: msg.ToolCalls})
		default:
			return nil, apierr.InvalidRequest(fmt.Sprintf("unsupported message role %q", msg.Role), fmt.Sprintf("messages.%d.role", i))
		}
	}
	return out, nil
}

// reportUnenforceableStrict reports tools whose strict: true this proxy
// accepted but cannot enforce, and applies the operator's policy for them.
//
// Accepting a control and silently not honouring it is the one outcome the
// validation policy rules out, and an uncompilable schema is not something the
// client can be 400'd for by default - an external $ref is refused a loader on
// purpose, and a freeform custom tool has no schema to compile at all, both of
// which real OpenAI accepts. Reporting is what keeps the acceptance honest:
// under best-effort the request succeeds and the operator can see that the
// guarantee was not applied.
//
// A warn log is only actionable for whoever reads the logs, though, and the
// client that trusted strict: true and skipped its own validation is the party
// actually exposed. COPILOT_STRICT_ENFORCEMENT=fail-closed gives an operator
// who cannot accept that the other trade: the request is refused with a 400
// naming every tool and reason, so the contract breaks loudly at the caller
// rather than quietly in a log file.
func (g *RealGateway) reportUnenforceableStrict(rt *toolproxy.RequestTools, surface string) error {
	if g == nil || rt == nil || len(rt.UnenforceableStrict) == 0 {
		return nil
	}
	failClosed := g.cfg.StrictEnforcement == config.StrictEnforcementFailClosed
	if g.log != nil {
		message := "accepted strict: true but cannot enforce it"
		if failClosed {
			message = "refused strict: true because it cannot be enforced"
		}
		for _, t := range rt.UnenforceableStrict {
			g.log.Warn(message, "surface", surface, "tool", t.Tool, "reason", t.Reason)
		}
	}
	if !failClosed {
		return nil
	}
	refusals := make([]string, 0, len(rt.UnenforceableStrict))
	for _, t := range rt.UnenforceableStrict {
		refusals = append(refusals, fmt.Sprintf("tool %q: %s", t.Tool, t.Reason))
	}
	return apierr.InvalidRequest(
		"this proxy cannot enforce strict: true for "+strings.Join(refusals, "; ")+
			" (COPILOT_STRICT_ENFORCEMENT=fail-closed refuses a strict contract it cannot honour; best-effort accepts it unenforced)",
		"tools")
}
