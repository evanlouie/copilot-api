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

func (g *RealGateway) Chat(ctx context.Context, req ChatRequest) (*TurnResult, error) {
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
	final, err := g.resolvePrompt(ctx, req.Model, finalPrompt, "messages")
	if err != nil {
		return nil, err
	}
	history, err := g.resolveChatHistory(ctx, req.Model, req.History)
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
		return nil, openai.Internal("failed to write synthetic session state: " + err.Error())
	}
	rt, err := toolproxy.NewRequestTools(g.broker, req.Tools, req.ToolChoiceNone)
	if err != nil {
		return nil, openai.InvalidRequest(err.Error(), "tools")
	}
	events := make(chan copilot.SessionEvent, 256)
	session, err := g.resumeSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, false, events)
	if err != nil {
		return nil, openai.Upstream(err.Error())
	}
	runner := g.newTurnRunner(ctx, req.OpenAIID, req.Model, session, rt, events, retained, "chat", "")
	runner.watchContext(ctx)
	if _, err := session.Send(ctx, copilot.MessageOptions{Prompt: final.Text, Attachments: final.Attachments}); err != nil {
		_ = session.Disconnect()
		return nil, openai.Upstream(err.Error())
	}
	result, err := runner.waitInitial(ctx)
	if err != nil {
		return nil, err
	}
	if result.PendingBatchID != "" {
		g.rememberRunner(result.PendingBatchID, runner)
	}
	_ = g.store.SaveSessionMetadata(sessionID, sessionstore.SessionMetadata{ID: sessionID, Kind: "chat", OpenAIID: result.ID, SDKSessionID: sessionID, Model: req.Model, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), RetainedPath: retained, FinishReason: result.FinishReason, PendingBatchID: result.PendingBatchID})
	return result, nil
}
func (g *RealGateway) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
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
	final, err := g.resolvePrompt(ctx, req.Model, finalPrompt, "messages")
	if err != nil {
		return nil, err
	}
	history, err := g.resolveChatHistory(ctx, req.Model, req.History)
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
		return nil, openai.Internal("failed to write synthetic session state: " + err.Error())
	}
	rt, err := toolproxy.NewRequestTools(g.broker, req.Tools, req.ToolChoiceNone)
	if err != nil {
		return nil, openai.InvalidRequest(err.Error(), "tools")
	}
	events := make(chan copilot.SessionEvent, 256)
	session, err := g.resumeSession(ctx, sessionID, req.Model, req.Instructions, reasoningEffort, rt, true, events)
	if err != nil {
		return nil, openai.Upstream(err.Error())
	}
	ch := make(chan StreamEvent, 32)
	runner := g.newTurnRunner(ctx, req.OpenAIID, req.Model, session, rt, events, retained, "chat", "")
	runner.watchContext(ctx)
	runner.enableChatStream(ch, ctx.Done())
	runner.setOnResult(func(result *TurnResult) error {
		if result.PendingBatchID != "" {
			g.rememberRunner(result.PendingBatchID, runner)
		}
		_ = g.store.SaveSessionMetadata(sessionID, sessionstore.SessionMetadata{ID: sessionID, Kind: "chat", OpenAIID: result.ID, SDKSessionID: sessionID, Model: req.Model, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), RetainedPath: retained, FinishReason: result.FinishReason, PendingBatchID: result.PendingBatchID})
		return nil
	})
	if _, err := session.Send(ctx, copilot.MessageOptions{Prompt: final.Text, Attachments: final.Attachments}); err != nil {
		_ = session.Disconnect()
		return nil, openai.Upstream(err.Error())
	}
	go runner.discardInitial()
	return ch, nil
}
func (g *RealGateway) resolveChatHistory(ctx context.Context, model string, messages []openai.ChatMessage) ([]hydration.Message, error) {
	out := make([]hydration.Message, 0, len(messages))
	for i, msg := range messages {
		switch msg.Role {
		case "user":
			prompt, err := msg.Prompt()
			if err != nil {
				return nil, openai.InvalidRequest(err.Error(), fmt.Sprintf("messages.%d.content", i))
			}
			resolved, err := g.resolvePrompt(ctx, model, prompt, fmt.Sprintf("messages.%d.content", i))
			if err != nil {
				return nil, err
			}
			out = append(out, hydration.Message{Role: msg.Role, Content: resolved.Text, Attachments: resolved.Attachments})
		case "assistant", "tool":
			text, err := msg.Text()
			if err != nil {
				return nil, openai.InvalidRequest(err.Error(), fmt.Sprintf("messages.%d.content", i))
			}
			out = append(out, hydration.Message{Role: msg.Role, Content: text, ToolCallID: msg.ToolCallID, ToolCalls: msg.ToolCalls})
		default:
			return nil, openai.InvalidRequest(fmt.Sprintf("unsupported message role %q", msg.Role), fmt.Sprintf("messages.%d.role", i))
		}
	}
	return out, nil
}
