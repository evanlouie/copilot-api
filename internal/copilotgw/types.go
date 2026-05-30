package copilotgw

import (
	"context"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
)

type Model struct {
	ID       string
	Name     string
	Metadata map[string]any
}

type Gateway interface {
	Start(ctx context.Context) error
	Stop() error
	Ready(ctx context.Context) error
	ListModels(ctx context.Context) ([]Model, error)
	ValidateModel(ctx context.Context, model string) error
	Chat(ctx context.Context, req ChatRequest) (*TurnResult, error)
	ContinueChatToolCalls(ctx context.Context, model string, outputs map[string]string) (*TurnResult, error)
	StreamContinueChatToolCalls(ctx context.Context, model string, outputs map[string]string) (<-chan StreamEvent, error)
	StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error)
	CreateResponse(ctx context.Context, req ResponseRequest) (*ResponseResult, error)
	StreamResponse(ctx context.Context, req ResponseRequest) (<-chan ResponseStreamEvent, error)
	GetResponse(ctx context.Context, id string) (*openai.Response, error)
	DeleteResponse(ctx context.Context, id string) error
}

type ChatRequest struct {
	OpenAIID          string
	Model             string
	Instructions      string
	History           []openai.ChatMessage
	FinalUser         openai.ChatMessage
	Tools             []openai.Tool
	ToolChoiceNone    bool
	ReasoningEffort   string
	IncludeUsageChunk bool
}

type TurnResult struct {
	ID             string
	Created        int64
	Model          string
	SDKSessionID   string
	Text           string
	Reasoning      string
	ToolCalls      []openai.ChatToolCall
	Usage          *openai.Usage
	FinishReason   string
	RetainedPath   string
	PendingBatchID string
}

type StreamEvent struct {
	Kind   string
	Delta  string
	Result *TurnResult
	Error  error
}

type ResponseRequest struct {
	ResponseID         string
	Model              string
	Instructions       string
	InputText          string
	FunctionOutputs    map[string]string
	PreviousResponseID string
	Tools              []openai.Tool
	ToolChoiceNone     bool
	Store              bool
	StoreSet           bool
	ReasoningEffort    string
}

type ResponseResult struct {
	Response *openai.Response
	Batch    *toolproxy.Batch
}

type ResponseStreamEvent struct {
	Kind     string
	Delta    string
	Response *openai.Response
	Item     *openai.ResponseOutputItem
	Error    error
}
