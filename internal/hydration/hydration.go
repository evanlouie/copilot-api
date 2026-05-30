package hydration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/google/uuid"
)

type Result struct {
	SessionID string
	Events    []copilot.SessionEvent
	JSONL     []byte
}

type Options struct {
	SessionID string
	Model     string
	Producer  string
	Now       time.Time
}

type Message struct {
	Role        string
	Content     string
	ToolCallID  string
	ToolCalls   []openai.ChatToolCall
	Attachments []copilot.Attachment
}

func BuildChatHistory(messages []openai.ChatMessage, opts Options) (Result, error) {
	inputs := make([]Message, 0, len(messages))
	for i, msg := range messages {
		text, err := msg.Text()
		if err != nil {
			return Result{}, fmt.Errorf("messages.%d.content: %w", i, err)
		}
		inputs = append(inputs, Message{Role: msg.Role, Content: text, ToolCallID: msg.ToolCallID, ToolCalls: msg.ToolCalls})
	}
	return BuildChatHistoryMessages(inputs, opts)
}

func BuildChatHistoryMessages(messages []Message, opts Options) (Result, error) {
	if opts.SessionID == "" {
		opts.SessionID = "chat_" + uuid.NewString()
	}
	if opts.Producer == "" {
		opts.Producer = "copilot-api"
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	b := builder{now: opts.Now}
	selectedModel := opts.Model
	b.add(copilot.SessionEventTypeSessionStart, &copilot.SessionStartData{
		CopilotVersion: "synthetic",
		Producer:       opts.Producer,
		SessionID:      opts.SessionID,
		StartTime:      opts.Now,
		Version:        1,
		SelectedModel:  &selectedModel,
	})

	toolDefs := map[string]openai.ChatToolCall{}
	turn := 0
	for i, msg := range messages {
		switch msg.Role {
		case "user":
			b.add(copilot.SessionEventTypeUserMessage, &copilot.UserMessageData{Content: msg.Content, Attachments: msg.Attachments})
		case "assistant":
			turnID := strconv.Itoa(turn)
			turn++
			b.add(copilot.SessionEventTypeAssistantTurnStart, &copilot.AssistantTurnStartData{TurnID: turnID})
			data := &copilot.AssistantMessageData{Content: msg.Content, MessageID: uuid.NewString()}
			for _, tc := range msg.ToolCalls {
				if tc.ID == "" {
					return Result{}, fmt.Errorf("messages.%d.tool_calls: tool call id is required", i)
				}
				args, err := decodeArguments(tc.Function.Arguments)
				if err != nil {
					return Result{}, fmt.Errorf("messages.%d.tool_calls.%s.arguments: %w", i, tc.ID, err)
				}
				typeFunction := copilot.AssistantMessageToolRequestTypeFunction
				data.ToolRequests = append(data.ToolRequests, copilot.AssistantMessageToolRequest{
					Name:       tc.Function.Name,
					ToolCallID: tc.ID,
					Type:       &typeFunction,
					Arguments:  args,
				})
				toolDefs[tc.ID] = tc
			}
			b.add(copilot.SessionEventTypeAssistantMessage, data)
			b.add(copilot.SessionEventTypeAssistantTurnEnd, &copilot.AssistantTurnEndData{TurnID: turnID})
		case "tool":
			prev, ok := toolDefs[msg.ToolCallID]
			if !ok {
				return Result{}, fmt.Errorf("messages.%d.tool_call_id %q does not match a previous assistant tool_call", i, msg.ToolCallID)
			}
			args, err := decodeArguments(prev.Function.Arguments)
			if err != nil {
				return Result{}, err
			}
			detail := msg.Content
			b.add(copilot.SessionEventTypeToolExecutionStart, &copilot.ToolExecutionStartData{ToolCallID: msg.ToolCallID, ToolName: prev.Function.Name, Arguments: args})
			b.add(copilot.SessionEventTypeToolExecutionComplete, &copilot.ToolExecutionCompleteData{ToolCallID: msg.ToolCallID, Success: true, Result: &copilot.ToolExecutionCompleteResult{Content: msg.Content, DetailedContent: &detail}})
		default:
			return Result{}, fmt.Errorf("messages.%d.role %q cannot be hydrated", i, msg.Role)
		}
	}
	jsonl, err := b.jsonl()
	if err != nil {
		return Result{}, err
	}
	return Result{SessionID: opts.SessionID, Events: b.events, JSONL: jsonl}, nil
}

func decodeArguments(s string) (any, error) {
	if s == "" {
		return map[string]any{}, nil
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

type builder struct {
	events []copilot.SessionEvent
	lastID *string
	now    time.Time
}

func (b *builder) add(t copilot.SessionEventType, data copilot.SessionEventData) {
	id := uuid.NewString()
	e := copilot.SessionEvent{ID: id, Timestamp: b.now.Add(time.Duration(len(b.events)) * time.Millisecond), ParentID: b.lastID, Type: t, Data: data}
	b.events = append(b.events, e)
	b.lastID = &b.events[len(b.events)-1].ID
}

func (b *builder) jsonl() ([]byte, error) {
	var out bytes.Buffer
	for i := range b.events {
		line, err := b.events[i].Marshal()
		if err != nil {
			return nil, err
		}
		if _, err := copilot.UnmarshalSessionEvent(line); err != nil {
			return nil, fmt.Errorf("generated invalid session event %s: %w", b.events[i].Type, err)
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	return out.Bytes(), nil
}
