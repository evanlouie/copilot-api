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

type ResponseUsage struct {
	InputTokens         *int64                       `json:"input_tokens,omitempty"`
	InputTokensDetails  *ResponseInputTokensDetails  `json:"input_tokens_details,omitempty"`
	OutputTokens        *int64                       `json:"output_tokens,omitempty"`
	OutputTokensDetails *ResponseOutputTokensDetails `json:"output_tokens_details,omitempty"`
	TotalTokens         *int64                       `json:"total_tokens,omitempty"`
}

type ResponseInputTokensDetails struct {
	CachedTokens *int64 `json:"cached_tokens,omitempty"`
}

type ResponseOutputTokensDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
}

func NewResponseUsage(usage *Usage) *ResponseUsage {
	if usage == nil {
		return nil
	}
	if usage.PromptTokens == nil && usage.CompletionTokens == nil && usage.TotalTokens == nil {
		return nil
	}
	out := &ResponseUsage{InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens}
	if out.TotalTokens == nil && out.InputTokens != nil && out.OutputTokens != nil {
		total := *out.InputTokens + *out.OutputTokens
		out.TotalTokens = &total
	}
	if usage.CompletionTokensDetails != nil && usage.CompletionTokensDetails.ReasoningTokens != nil {
		out.OutputTokensDetails = &ResponseOutputTokensDetails{ReasoningTokens: usage.CompletionTokensDetails.ReasoningTokens}
	}
	return out
}

func (u *ResponseUsage) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err == nil {
		if _, ok := raw["prompt_tokens"]; ok {
			return u.unmarshalLegacyJSON(data)
		}
		if _, ok := raw["completion_tokens"]; ok {
			return u.unmarshalLegacyJSON(data)
		}
		if _, ok := raw["completion_tokens_details"]; ok {
			return u.unmarshalLegacyJSON(data)
		}
	}
	type alias ResponseUsage
	var current alias
	if err := json.Unmarshal(data, &current); err != nil {
		return err
	}
	*u = ResponseUsage(current)
	return nil
}

func (u *ResponseUsage) unmarshalLegacyJSON(data []byte) error {
	var legacy Usage
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	if converted := NewResponseUsage(&legacy); converted != nil {
		*u = *converted
		return nil
	}
	*u = ResponseUsage{}
	return nil
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

func (m ChatMessage) Prompt() (PromptContent, error) {
	return m.Content.Prompt()
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

type PromptContent struct {
	Text   string
	Images []ImageInput
}

type ImageInput struct {
	URL    string
	Detail string
}

func (c Content) Text() (string, error) {
	prompt, err := c.Prompt()
	if err != nil {
		return "", err
	}
	if len(prompt.Images) > 0 {
		return "", fmt.Errorf("image content is not supported in text-only content")
	}
	return prompt.Text, nil
}

func (c Content) Prompt() (PromptContent, error) {
	if !c.Present || c.IsNull {
		return PromptContent{}, nil
	}
	var s string
	if err := json.Unmarshal(c.Raw, &s); err == nil {
		return PromptContent{Text: s}, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(c.Raw, &parts); err == nil {
		var prompt PromptContent
		var b strings.Builder
		for _, p := range parts {
			switch p.Type {
			case "text", "input_text", "output_text":
				b.WriteString(p.Text)
			case "refusal":
				if p.Refusal != "" {
					b.WriteString(p.Refusal)
				}
			case "image_url", "input_image":
				image, err := p.Image()
				if err != nil {
					return PromptContent{}, err
				}
				prompt.Images = append(prompt.Images, image)
			default:
				return PromptContent{}, fmt.Errorf("unsupported content part type %q", p.Type)
			}
		}
		prompt.Text = b.String()
		return prompt, nil
	}
	return PromptContent{}, fmt.Errorf("content must be a string or content parts")
}

type ContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Refusal  string          `json:"refusal,omitempty"`
	ImageURL json.RawMessage `json:"image_url,omitempty"`
	FileID   string          `json:"file_id,omitempty"`
	Detail   string          `json:"detail,omitempty"`
}

func (p ContentPart) Image() (ImageInput, error) {
	if p.FileID != "" {
		return ImageInput{}, fmt.Errorf("file_id image inputs are not supported")
	}
	if len(p.ImageURL) == 0 || string(p.ImageURL) == "null" {
		return ImageInput{}, fmt.Errorf("%s content parts require image_url", p.Type)
	}
	var url string
	if err := json.Unmarshal(p.ImageURL, &url); err == nil {
		return ImageInput{URL: url, Detail: p.Detail}, nil
	}
	var obj struct {
		URL    string `json:"url"`
		Detail string `json:"detail,omitempty"`
	}
	if err := json.Unmarshal(p.ImageURL, &obj); err != nil {
		return ImageInput{}, fmt.Errorf("image_url must be a string or object")
	}
	detail := p.Detail
	if detail == "" {
		detail = obj.Detail
	}
	if obj.URL == "" {
		return ImageInput{}, fmt.Errorf("image_url.url is required")
	}
	return ImageInput{URL: obj.URL, Detail: detail}, nil
}

type Tool struct {
	Type     string       `json:"type"`
	Function FunctionTool `json:"function"`
}

func (t *Tool) UnmarshalJSON(data []byte) error {
	type alias Tool
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	if a.Type == "function" && a.Function.Name == "" {
		var flat struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			Parameters  json.RawMessage `json:"parameters,omitempty"`
			Strict      *bool           `json:"strict,omitempty"`
		}
		if err := json.Unmarshal(data, &flat); err != nil {
			return err
		}
		a.Function = FunctionTool{Name: flat.Name, Description: flat.Description, Parameters: flat.Parameters, Strict: flat.Strict}
	}
	*t = Tool(a)
	return nil
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
	IncludeUsage      bool              `json:"-"`
}

func (c ChatCompletionChunk) MarshalJSON() ([]byte, error) {
	type alias ChatCompletionChunk
	if !c.IncludeUsage {
		return json.Marshal(alias(c))
	}
	// When stream_options.include_usage is set, OpenAI sends usage on every
	// chunk (null until the terminal usage chunk). Embedding the alias keeps all
	// other fields in sync automatically while the shadowing Usage field drops
	// omitempty so null is rendered explicitly.
	return json.Marshal(struct {
		alias
		Usage *Usage `json:"usage"`
	}{alias: alias(c), Usage: c.Usage})
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
	Include            json.RawMessage `json:"include,omitempty"`
	Reasoning          json.RawMessage `json:"reasoning,omitempty"`
	Text               json.RawMessage `json:"text,omitempty"`
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
	Usage              *ResponseUsage       `json:"usage,omitempty"`
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
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

func (t ResponseText) MarshalJSON() ([]byte, error) {
	type responseText struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Annotations []any  `json:"annotations"`
	}
	annotations := t.Annotations
	if annotations == nil {
		annotations = []any{}
	}
	return json.Marshal(responseText{Type: t.Type, Text: t.Text, Annotations: annotations})
}

type ResponseInputItem struct {
	Type      string          `json:"type,omitempty"`
	ID        string          `json:"id,omitempty"`
	Role      string          `json:"role,omitempty"`
	Content   Content         `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Input     string          `json:"input,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
}

type ResponseStreamEvent struct {
	EventID        string              `json:"event_id,omitempty"`
	Type           string              `json:"type"`
	SequenceNumber int64               `json:"sequence_number"`
	Response       *Response           `json:"response,omitempty"`
	Item           *ResponseOutputItem `json:"item,omitempty"`
	Part           *ResponseText       `json:"part,omitempty"`
	ItemID         string              `json:"item_id,omitempty"`
	OutputIndex    *int                `json:"output_index,omitempty"`
	ContentIndex   *int                `json:"content_index,omitempty"`
	Delta          string              `json:"delta,omitempty"`
	Text           string              `json:"text,omitempty"`
	Arguments      string              `json:"arguments,omitempty"`
	Name           string              `json:"name,omitempty"`
	Status         string              `json:"status,omitempty"`
	Error          *ErrorObject        `json:"error,omitempty"`
}
