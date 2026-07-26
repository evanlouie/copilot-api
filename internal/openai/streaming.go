package openai

import "encoding/json"

// Streaming wire DTOs: Chat Completions chunks and Responses stream events.

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
	Role             string            `json:"role,omitempty"`
	Content          string            `json:"content,omitempty"`
	Reasoning        string            `json:"reasoning,omitempty"`
	ReasoningContent string            `json:"reasoning_content,omitempty"`
	ReasoningDetails []ReasoningDetail `json:"reasoning_details,omitempty"`
	ToolCalls        []ToolCallDelta   `json:"tool_calls,omitempty"`
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

type ResponseStreamEvent struct {
	EventID        string              `json:"event_id,omitempty"`
	Type           string              `json:"type"`
	SequenceNumber int64               `json:"sequence_number"`
	Response       *Response           `json:"response,omitempty"`
	Item           *ResponseOutputItem `json:"item,omitempty"`
	// Part carries either a content part (*ResponseText) or a reasoning summary
	// part (ResponseReasoningSummary), depending on the event type.
	Part         any          `json:"part,omitempty"`
	ItemID       string       `json:"item_id,omitempty"`
	OutputIndex  *int         `json:"output_index,omitempty"`
	ContentIndex *int         `json:"content_index,omitempty"`
	SummaryIndex *int         `json:"summary_index,omitempty"`
	Delta        string       `json:"delta,omitempty"`
	Text         string       `json:"text,omitempty"`
	Arguments    string       `json:"arguments,omitempty"`
	Name         string       `json:"name,omitempty"`
	Status       string       `json:"status,omitempty"`
	Error        *ErrorObject `json:"error,omitempty"`
}
