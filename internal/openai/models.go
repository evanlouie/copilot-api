package openai

import (
	"time"

	"github.com/google/uuid"
)

// Model, usage and object-kind wire DTOs shared by both APIs.

const (
	ObjectList           = "list"
	ObjectModel          = "model"
	ObjectChatCompletion = "chat.completion"
	ObjectChatChunk      = "chat.completion.chunk"
	ObjectResponse       = "response"
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
