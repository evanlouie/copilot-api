package openai

import (
	"bytes"
	"encoding/json"
)

// Chat Completions wire DTOs.

type ChatCompletionRequest struct {
	Model               string                     `json:"model"`
	Messages            []ChatMessage              `json:"messages"`
	Stream              bool                       `json:"stream,omitempty"`
	Tools               []Tool                     `json:"tools,omitempty"`
	ToolChoice          json.RawMessage            `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool                      `json:"parallel_tool_calls,omitempty"`
	StreamOptions       *StreamOptions             `json:"stream_options,omitempty"`
	Temperature         *float64                   `json:"temperature,omitempty"`
	TopP                *float64                   `json:"top_p,omitempty"`
	PresencePenalty     *float64                   `json:"presence_penalty,omitempty"`
	FrequencyPenalty    *float64                   `json:"frequency_penalty,omitempty"`
	MaxTokens           *int                       `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                       `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string                     `json:"reasoning_effort,omitempty"`
	Raw                 map[string]json.RawMessage `json:"-"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

func (r *ChatCompletionRequest) UnmarshalJSON(data []byte) error {
	var wire struct {
		Model               string          `json:"model"`
		Messages            []ChatMessage   `json:"messages"`
		Stream              bool            `json:"stream,omitempty"`
		Tools               []Tool          `json:"tools,omitempty"`
		ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
		ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
		StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
		ReasoningEffort     string          `json:"reasoning_effort,omitempty"`
		Audio               json.RawMessage `json:"audio"`
		FunctionCall        json.RawMessage `json:"function_call"`
		Functions           json.RawMessage `json:"functions"`
		LogitBias           json.RawMessage `json:"logit_bias"`
		Logprobs            json.RawMessage `json:"logprobs"`
		TopLogprobs         json.RawMessage `json:"top_logprobs"`
		MaxTokens           json.RawMessage `json:"max_tokens"`
		MaxCompletionTokens json.RawMessage `json:"max_completion_tokens"`
		Modalities          json.RawMessage `json:"modalities"`
		Prediction          json.RawMessage `json:"prediction"`
		ResponseFormat      json.RawMessage `json:"response_format"`
		Stop                json.RawMessage `json:"stop"`
		N                   json.RawMessage `json:"n"`
		Temperature         json.RawMessage `json:"temperature"`
		TopP                json.RawMessage `json:"top_p"`
		PresencePenalty     json.RawMessage `json:"presence_penalty"`
		FrequencyPenalty    json.RawMessage `json:"frequency_penalty"`
		Seed                json.RawMessage `json:"seed"`
		Metadata            json.RawMessage `json:"metadata"`
		ServiceTier         json.RawMessage `json:"service_tier"`
		User                json.RawMessage `json:"user"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*r = ChatCompletionRequest{
		Model: wire.Model, Messages: wire.Messages, Stream: wire.Stream, Tools: wire.Tools,
		ToolChoice: wire.ToolChoice, ParallelToolCalls: wire.ParallelToolCalls,
		StreamOptions: wire.StreamOptions, ReasoningEffort: wire.ReasoningEffort,
	}
	if err := decodeOptionalRaw(wire.Temperature, &r.Temperature); err != nil {
		return err
	}
	if err := decodeOptionalRaw(wire.TopP, &r.TopP); err != nil {
		return err
	}
	if err := decodeOptionalRaw(wire.PresencePenalty, &r.PresencePenalty); err != nil {
		return err
	}
	if err := decodeOptionalRaw(wire.FrequencyPenalty, &r.FrequencyPenalty); err != nil {
		return err
	}
	if err := decodeOptionalRaw(wire.MaxTokens, &r.MaxTokens); err != nil {
		return err
	}
	if err := decodeOptionalRaw(wire.MaxCompletionTokens, &r.MaxCompletionTokens); err != nil {
		return err
	}
	r.Raw = presentRawFields(map[string]json.RawMessage{
		"audio": wire.Audio, "function_call": wire.FunctionCall, "functions": wire.Functions,
		"logit_bias": wire.LogitBias, "logprobs": wire.Logprobs, "top_logprobs": wire.TopLogprobs,
		"max_tokens": wire.MaxTokens, "max_completion_tokens": wire.MaxCompletionTokens,
		"modalities": wire.Modalities, "prediction": wire.Prediction, "response_format": wire.ResponseFormat,
		"stop": wire.Stop, "n": wire.N, "temperature": wire.Temperature, "top_p": wire.TopP,
		"presence_penalty": wire.PresencePenalty, "frequency_penalty": wire.FrequencyPenalty,
		"seed": wire.Seed, "metadata": wire.Metadata, "service_tier": wire.ServiceTier, "user": wire.User,
	})
	return nil
}

func decodeOptionalRaw[T any](raw json.RawMessage, dst **T) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*dst = &value
	return nil
}

type ChatMessage struct {
	Role             string            `json:"role"`
	Content          Content           `json:"content,omitempty"`
	Name             string            `json:"name,omitempty"`
	ToolCallID       string            `json:"tool_call_id,omitempty"`
	ToolCalls        []ChatToolCall    `json:"tool_calls,omitempty"`
	Reasoning        string            `json:"reasoning,omitempty"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ReasoningDetails []ReasoningDetail `json:"reasoning_details,omitempty"`
	Refusal          *string           `json:"refusal,omitempty"`
	Raw              json.RawMessage   `json:"-"`
}

// ReasoningDetail is a structured reasoning block following the de-facto
// OpenRouter/Anthropic `reasoning_details` convention. The fields are emitted
// with omitempty semantics so each block only carries the keys meaningful for
// its type (`reasoning.text` carries text/signature, `reasoning.encrypted`
// carries data). Inbound blocks are tolerated for continuity round-trips.
type ReasoningDetail struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
	Summary   string `json:"summary,omitempty"`
	ID        string `json:"id,omitempty"`
	Format    string `json:"format,omitempty"`
	Index     *int   `json:"index,omitempty"`
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
