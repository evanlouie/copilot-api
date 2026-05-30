package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	ObjectList              = "list"
	ObjectModel             = "model"
	ObjectChatCompletion    = "chat.completion"
	ObjectChatChunk         = "chat.completion.chunk"
	ObjectResponse          = "response"
	ObjectResponseItem      = "response.output_item"
	ObjectResponseTextDelta = "response.output_text.delta"
)

func NewID(prefix string) string { return prefix + uuid.NewString() }
func UnixNow() int64             { return time.Now().Unix() }

type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

type Model struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	OwnedBy string         `json:"owned_by"`
	Meta    map[string]any `json:"metadata,omitempty"`
}

type Usage struct {
	PromptTokens            *int64        `json:"prompt_tokens,omitempty"`
	CompletionTokens        *int64        `json:"completion_tokens,omitempty"`
	TotalTokens             *int64        `json:"total_tokens,omitempty"`
	CompletionTokensDetails *TokenDetails `json:"completion_tokens_details,omitempty"`
}

type TokenDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
}

type ChatCompletionRequest struct {
	Model               string          `json:"model"`
	Messages            []ChatMessage   `json:"messages"`
	Stream              bool            `json:"stream,omitempty"`
	Tools               []Tool          `json:"tools,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	PresencePenalty     *float64        `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64        `json:"frequency_penalty,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
	Raw                 map[string]any  `json:"-"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

func (r *ChatCompletionRequest) UnmarshalJSON(data []byte) error {
	type alias ChatCompletionRequest
	var a alias
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&a); err != nil {
		return err
	}
	var raw map[string]any
	dec = json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	*r = ChatCompletionRequest(a)
	r.Raw = raw
	return nil
}

type ChatMessage struct {
	Role       string          `json:"role"`
	Content    Content         `json:"content,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []ChatToolCall  `json:"tool_calls,omitempty"`
	Refusal    *string         `json:"refusal,omitempty"`
	Raw        json.RawMessage `json:"-"`
}

func (m *ChatMessage) UnmarshalJSON(data []byte) error {
	type alias ChatMessage
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*m = ChatMessage(a)
	m.Raw = append(m.Raw[:0], data...)
	return nil
}

func (m ChatMessage) Text() (string, error) {
	return m.Content.Text()
}

type Content struct {
	Raw     json.RawMessage
	IsNull  bool
	Present bool
}

func (c *Content) UnmarshalJSON(data []byte) error {
	c.Present = true
	c.Raw = append(c.Raw[:0], data...)
	c.IsNull = bytes.Equal(bytes.TrimSpace(data), []byte("null"))
	return nil
}

func (c Content) MarshalJSON() ([]byte, error) {
	if !c.Present || c.IsNull {
		return []byte("null"), nil
	}
	return c.Raw, nil
}

func NewTextContent(s string) Content {
	b, _ := json.Marshal(s)
	return Content{Raw: b, Present: true}
}

func (c Content) Text() (string, error) {
	if !c.Present || c.IsNull {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(c.Raw, &s); err == nil {
		return s, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(c.Raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			switch p.Type {
			case "text", "input_text", "output_text":
				b.WriteString(p.Text)
			case "refusal":
				if p.Refusal != "" {
					b.WriteString(p.Refusal)
				}
			default:
				return "", fmt.Errorf("unsupported content part type %q", p.Type)
			}
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("content must be a string or text content parts")
}

type ContentPart struct {
	Type    string `json:"type"`
	Text    string `json:"text,omitempty"`
	Refusal string `json:"refusal,omitempty"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function FunctionTool `json:"function"`
}

type FunctionTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type ChatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ChatCompletion struct {
	ID                string                 `json:"id"`
	Object            string                 `json:"object"`
	Created           int64                  `json:"created"`
	Model             string                 `json:"model"`
	Choices           []ChatCompletionChoice `json:"choices"`
	Usage             *Usage                 `json:"usage,omitempty"`
	SystemFingerprint *string                `json:"system_fingerprint"`
}

type ChatCompletionChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type ChatCompletionChunk struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	Created           int64             `json:"created"`
	Model             string            `json:"model"`
	Choices           []ChatChunkChoice `json:"choices"`
	Usage             *Usage            `json:"usage,omitempty"`
	SystemFingerprint *string           `json:"system_fingerprint"`
}

type ChatChunkChoice struct {
	Index        int            `json:"index"`
	Delta        ChatChunkDelta `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

type ChatChunkDelta struct {
	Role      string          `json:"role,omitempty"`
	Content   string          `json:"content,omitempty"`
	ToolCalls []ToolCallDelta `json:"tool_calls,omitempty"`
}

type ToolCallDelta struct {
	Index    int                    `json:"index"`
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type,omitempty"`
	Function *ToolCallDeltaFunction `json:"function,omitempty"`
}

type ToolCallDeltaFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type ResponsesRequest struct {
	Model              string          `json:"model"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions,omitempty"`
	PreviousResponseID string          `json:"previous_response_id,omitempty"`
	Stream             bool            `json:"stream,omitempty"`
	Tools              []Tool          `json:"tools,omitempty"`
	ToolChoice         json.RawMessage `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool           `json:"parallel_tool_calls,omitempty"`
	Store              *bool           `json:"store,omitempty"`
	ReasoningEffort    string          `json:"reasoning_effort,omitempty"`
	Raw                map[string]any  `json:"-"`
}

func (r *ResponsesRequest) UnmarshalJSON(data []byte) error {
	type alias ResponsesRequest
	var a alias
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&a); err != nil {
		return err
	}
	var raw map[string]any
	dec = json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	*r = ResponsesRequest(a)
	r.Raw = raw
	return nil
}

type Response struct {
	ID                 string               `json:"id"`
	Object             string               `json:"object"`
	CreatedAt          int64                `json:"created_at"`
	Status             string               `json:"status"`
	Model              string               `json:"model"`
	Instructions       string               `json:"instructions,omitempty"`
	Output             []ResponseOutputItem `json:"output"`
	OutputText         string               `json:"output_text"`
	ParallelToolCalls  bool                 `json:"parallel_tool_calls"`
	PreviousResponseID *string              `json:"previous_response_id"`
	Store              bool                 `json:"store"`
	Usage              *Usage               `json:"usage,omitempty"`
	Error              any                  `json:"error"`
	IncompleteDetails  any                  `json:"incomplete_details"`
	Metadata           map[string]string    `json:"metadata,omitempty"`
}

type ResponseOutputItem struct {
	ID        string          `json:"id,omitempty"`
	Type      string          `json:"type"`
	Status    string          `json:"status,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   []ResponseText  `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
}

type ResponseText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ResponseInputItem struct {
	Type    string          `json:"type,omitempty"`
	Role    string          `json:"role,omitempty"`
	Content Content         `json:"content,omitempty"`
	CallID  string          `json:"call_id,omitempty"`
	Output  json.RawMessage `json:"output,omitempty"`
}

type ResponseStreamEvent struct {
	Type         string              `json:"type"`
	Response     *Response           `json:"response,omitempty"`
	Item         *ResponseOutputItem `json:"item,omitempty"`
	ItemID       string              `json:"item_id,omitempty"`
	OutputIndex  *int                `json:"output_index,omitempty"`
	ContentIndex *int                `json:"content_index,omitempty"`
	Delta        string              `json:"delta,omitempty"`
	Arguments    string              `json:"arguments,omitempty"`
	Error        *ErrorObject        `json:"error,omitempty"`
}
