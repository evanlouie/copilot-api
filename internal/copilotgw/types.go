package copilotgw

import (
	"context"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
)

type Model struct {
	ID                        string
	Name                      string
	Metadata                  map[string]any
	Limits                    *TokenLimits
	VisionKnown               bool
	SupportsVision            bool
	Vision                    *VisionLimits
	ReasoningEffortKnown      bool
	SupportsReasoningEffort   bool
	SupportedReasoningEfforts []string
	DefaultReasoningEffort    string
}

type TokenLimits struct {
	MaxContextWindowTokens *int64
	MaxPromptTokens        *int64
	MaxOutputTokens        *int64
}

type VisionLimits struct {
	SupportedMediaTypes []string
	MaxPromptImages     int64
	MaxPromptImageSize  int64
}

type Gateway interface {
	Start(ctx context.Context) error
	Stop() error
	Ready(ctx context.Context) error
	ListModels(ctx context.Context) ([]Model, error)
	ValidateModel(ctx context.Context, model string) error
	Chat(ctx context.Context, req ChatRequest) (*TurnResult, error)
	ContinueChatToolCalls(ctx context.Context, req ChatContinuationRequest) (*TurnResult, error)
	StreamContinueChatToolCalls(ctx context.Context, req ChatContinuationRequest) (<-chan StreamEvent, error)
	StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error)
	CreateResponse(ctx context.Context, req ResponseRequest) (*ResponseResult, error)
	WarmResponse(ctx context.Context, req ResponseRequest) (*WarmResponseResult, error)
	StreamResponse(ctx context.Context, req ResponseRequest) (<-chan ResponseStreamEvent, error)
	GetResponse(ctx context.Context, id string) (*openai.Response, error)
	DeleteResponse(ctx context.Context, id string) error
}

type ChatRequest struct {
	OpenAIID                string
	Model                   string
	Instructions            string
	History                 []openai.ChatMessage
	FinalUser               openai.ChatMessage
	Tools                   []openai.Tool
	ToolChoiceNone          bool
	ReasoningEffort         string
	DefaultReasoningEffort  string
	ResolvedReasoningEffort string
	ReasoningEffortResolved bool
	IncludeUsageChunk       bool
}

type ChatContinuationRequest struct {
	Model                  string
	Instructions           string
	Messages               []openai.ChatMessage
	Outputs                map[string]string
	Tools                  []openai.Tool
	ToolChoiceNone         bool
	ReasoningEffort        string
	DefaultReasoningEffort string
	IncludeUsageChunk      bool
}

type TurnResult struct {
	ID                 string
	Created            int64
	Model              string
	SDKSessionID       string
	Text               string
	Reasoning          string
	ReasoningOpaque    string
	ReasoningEncrypted string
	ReasoningID        string
	ToolCalls          []openai.ChatToolCall
	Usage              *openai.Usage
	FinishReason       string
	RetainedPath       string
	PendingBatchID     string
}

type StreamEvent struct {
	Kind        string
	Delta       string
	ReasoningID string
	Result      *TurnResult
	Error       error
}

type ResponseRequest struct {
	ResponseID                         string
	Model                              string
	Instructions                       string
	Input                              openai.PromptContent
	FunctionOutputs                    map[string]string
	FunctionOutputFallbackInput        openai.PromptContent
	FunctionOutputFallbackInstructions string
	FunctionOutputFallbackAvailable    bool
	PreviousResponseID                 string
	WarmSession                        *WarmResponseSession
	Tools                              []openai.Tool
	ToolChoiceNone                     bool
	Store                              bool
	StoreSet                           bool
	ReasoningEffort                    string
	DefaultReasoningEffort             string
	ResolvedReasoningEffort            string
	ReasoningEffortResolved            bool
	SuppressReasoning                  bool
}

type ResponseResult struct {
	Response *openai.Response
	Batch    *toolproxy.Batch
}

type WarmResponseResult struct {
	Response    *openai.Response
	WarmSession *WarmResponseSession
}

type ResponseStreamEvent struct {
	Kind        string
	Delta       string
	ReasoningID string
	Response    *openai.Response
	Item        *openai.ResponseOutputItem
	Error       error
}
