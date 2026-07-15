package hydration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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
	Reasoning   string
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
		inputs = append(inputs, Message{Role: msg.Role, Content: text, Reasoning: msg.InboundReasoning(), ToolCallID: msg.ToolCallID, ToolCalls: msg.ToolCalls})
	}
	return BuildChatHistoryMessages(inputs, opts)
}

func BuildChatHistoryMessages(messages []Message, opts Options) (Result, error) {
	return buildChatHistoryMessages(messages, opts, true, true)
}

func BuildChatHistoryJSONL(messages []Message, opts Options) (Result, error) {
	return buildChatHistoryMessages(messages, opts, false, false)
}

func buildChatHistoryMessages(messages []Message, opts Options, retainEvents, validateEvents bool) (Result, error) {
	if opts.SessionID == "" {
		opts.SessionID = "chat_" + uuid.NewString()
	}
	if opts.Producer == "" {
		opts.Producer = "copilot-api"
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	b := builder{now: opts.Now, retainEvents: retainEvents, validateEvents: validateEvents}
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
			if msg.Reasoning != "" {
				// Replay plaintext reasoning when reconstructing a cold session.
				// Opaque/encrypted reasoning is session-bound and stripped by the
				// SDK on resume, so only the readable text round-trips here.
				reasoning := msg.Reasoning
				data.ReasoningText = &reasoning
			}
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
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("arguments must contain a single JSON value")
	}
	return v, nil
}

type builder struct {
	events         []copilot.SessionEvent
	lastID         *string
	now            time.Time
	count          int
	out            bytes.Buffer
	err            error
	retainEvents   bool
	validateEvents bool
}

func (b *builder) add(t copilot.SessionEventType, data copilot.SessionEventData) {
	if b.err != nil {
		return
	}
	id := uuid.NewString()
	e := copilot.SessionEvent{ID: id, Timestamp: b.now.Add(time.Duration(b.count) * time.Millisecond), ParentID: b.lastID, Data: data}
	b.count++
	line, err := e.Marshal()
	if err != nil {
		b.err = err
		return
	}
	if b.validateEvents {
		var decoded copilot.SessionEvent
		if err := json.Unmarshal(line, &decoded); err != nil {
			b.err = fmt.Errorf("generated invalid session event %s: %w", e.Type(), err)
			return
		}
	}
	b.out.Write(line)
	b.out.WriteByte('\n')
	if b.retainEvents {
		b.events = append(b.events, e)
	}
	lastID := id
	b.lastID = &lastID
}

func (b *builder) jsonl() ([]byte, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.out.Bytes(), nil
}
