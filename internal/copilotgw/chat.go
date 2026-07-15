package copilotgw

import (
	"context"
	"fmt"
	"time"

	"github.com/evanlouie/copilot-api/internal/hydration"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionfs"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/google/uuid"
)

type preparedChatTurn struct {
	sessionID string
	retained  string
	final     resolvedPrompt
	rt        *toolproxy.RequestTools
	events    chan copilot.SessionEvent
	session   *copilot.Session
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
		return nil, openai.InvalidRequest(err.Error(), "messages")
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
	h, err := hydration.BuildChatHistoryMessages(history, hydration.Options{SessionID: sessionID, Model: req.Model})
	if err != nil {
		return nil, openai.InvalidRequest("failed to hydrate chat history: "+err.Error(), "messages")
	}
	retained, err := sessionfs.WriteEvents(g.cfg.DataDir, sessionID, h.JSONL)
	if err != nil {
		return nil, openai.Internal("failed to write synthetic session state")
	}
	rt, err := toolproxy.NewRequestTools(g.broker, req.Tools, req.ToolChoiceNone)
	if err != nil {
		return nil, openai.InvalidRequest(err.Error(), "tools")
	}
	events := make(chan copilot.SessionEvent, 256)
	session, err := g.resumeSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, streaming, events)
	if err != nil {
		return nil, openai.Upstream(err.Error())
	}
	if session == nil {
		return nil, openai.Upstream("copilot SDK returned nil session")
	}
	return &preparedChatTurn{sessionID: sessionID, retained: retained, final: final, rt: rt, events: events, session: session}, nil
}

func (g *RealGateway) Chat(ctx context.Context, req ChatRequest) (*TurnResult, error) {
	prepared, err := g.prepareChatTurn(ctx, req, false)
	if err != nil {
		return nil, err
	}
	runner := g.newTurnRunner(ctx, req.OpenAIID, req.Model, prepared.session, prepared.rt, prepared.events, prepared.retained, "chat", "")
	runner.watchContext(ctx)
	if _, err := prepared.session.Send(ctx, copilot.MessageOptions{Prompt: prepared.final.Text, Attachments: prepared.final.Attachments}); err != nil {
		runner.failSend(prepared.events, err)
		_, _ = runner.waitInitial(ctx)
		return nil, openai.Upstream(err.Error())
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
	runner := g.newTurnRunner(ctx, req.OpenAIID, req.Model, prepared.session, prepared.rt, prepared.events, prepared.retained, "chat", "")
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
				return nil, openai.InvalidRequest(err.Error(), fmt.Sprintf("messages.%d.content", i))
			}
			resolved, err := g.resolvePromptWithImageBudget(ctx, model, prompt, fmt.Sprintf("messages.%d.content", i), imageBudget)
			if err != nil {
				return nil, err
			}
			out = append(out, hydration.Message{Role: msg.Role, Content: resolved.Text, Attachments: resolved.Attachments})
		case "assistant", "tool":
			text, err := msg.Text()
			if err != nil {
				return nil, openai.InvalidRequest(err.Error(), fmt.Sprintf("messages.%d.content", i))
			}
			out = append(out, hydration.Message{Role: msg.Role, Content: text, Reasoning: msg.InboundReasoning(), ToolCallID: msg.ToolCallID, ToolCalls: msg.ToolCalls})
		default:
			return nil, openai.InvalidRequest(fmt.Sprintf("unsupported message role %q", msg.Role), fmt.Sprintf("messages.%d.role", i))
		}
	}
	return out, nil
}
