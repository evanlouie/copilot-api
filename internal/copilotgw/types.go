package copilotgw

import (
	"context"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
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

type LifecycleGateway interface {
	Start(ctx context.Context) error
	Stop() error
}

type ModelGateway interface {
	Ready(ctx context.Context) error
	ListModels(ctx context.Context) ([]Model, error)
	ValidateModel(ctx context.Context, model string) error
}

type ChatGateway interface {
	Chat(ctx context.Context, req ChatRequest) (*TurnResult, error)
	ContinueChatToolCalls(ctx context.Context, req ChatContinuationRequest) (*TurnResult, error)
	StreamContinueChatToolCalls(ctx context.Context, req ChatContinuationRequest) (<-chan StreamEvent, error)
	StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error)
}

type ResponsesGateway interface {
	CreateResponse(ctx context.Context, req ResponseRequest) (*ResponseResult, error)
	WarmResponse(ctx context.Context, req ResponseRequest) (*WarmResponseResult, error)
	StreamResponse(ctx context.Context, req ResponseRequest) (<-chan ResponseStreamEvent, error)
	GetResponse(ctx context.Context, id string) (*openai.Response, error)
	DeleteResponse(ctx context.Context, id string) error
}

type HTTPGateway interface {
	ModelGateway
	ChatGateway
	ResponsesGateway
}

type Gateway interface {
	LifecycleGateway
	HTTPGateway
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
	ResponseToolCalls  []toolproxy.CapturedCall
	Usage              *openai.Usage
	FinishReason       string
	RetainedPath       string
	PendingBatchID     string
	// MessageItemID and ReasoningItemID are the Responses output-item IDs for
	// this turn. The runner assigns them once, before the first streamed delta
	// carries them to the client, so the streamed events, the terminal response
	// and the persisted record all name the same items.
	MessageItemID   string
	ReasoningItemID string
	// Response is the single openai.Response built for this result. It is
	// constructed exactly once (see turnRunner.buildTurnResponse) and shared by
	// persistence, the streamed terminal event and the non-streaming JSON body.
	Response *openai.Response

	// itemOrder is the order in which this turn's output items were first
	// announced on the response stream. It is the single source of truth for
	// output_index: responseFromTurn arranges Response.Output to match, so a
	// streamed item's index always equals its position in the stored record.
	itemOrder []string
	// responseBuilds counts how many openai.Response values were constructed
	// from this result. Exactly one construction per turn is the invariant that
	// keeps every transport and the store in agreement; tests assert on it.
	responseBuilds int
}

// StreamedOutputItemOrder reports the order in which this turn's output items
// were announced on the response stream. It exists for tests that assert the
// streamed output_index matches the persisted output order.
func (t *TurnResult) StreamedOutputItemOrder() []string {
	return append([]string(nil), t.itemOrder...)
}

// ResponseBuilds reports how many openai.Response values were built from this
// result. It must be exactly one for any turn that produced a response.
func (t *TurnResult) ResponseBuilds() int { return t.responseBuilds }

type StreamEvent struct {
	Kind        string
	Delta       string
	ReasoningID string
	Result      *TurnResult
	Error       error
}

type ResponseRequest struct {
	ResponseID string
	// CreatedAt is the response's creation time, stamped once when the request
	// is prepared. Every frame that carries the response - response.created at
	// the start of a stream, the terminal response.completed, and the stored
	// record - uses it, so one response never reports two creation times.
	CreatedAt                          int64
	Model                              string
	Instructions                       string
	Input                              openai.PromptContent
	ToolOutputs                        map[string]toolcatalog.ResponseToolOutput
	FunctionOutputFallbackInput        openai.PromptContent
	FunctionOutputFallbackInstructions string
	FunctionOutputFallbackAvailable    bool
	PreviousResponseID                 string
	WarmSession                        *WarmResponseSession
	Tools                              []toolcatalog.NormalizedTool
	ToolsSet                           bool
	ToolChoiceNone                     bool
	ForceSynthetic                     bool
	ContinuationToolOutputs            map[string]toolcatalog.ResponseToolOutput
	LoadedToolEvents                   []toolcatalog.StoredLoadedToolEvent
	Store                              bool
	StoreSet                           bool
	ReasoningEffort                    string
	DefaultReasoningEffort             string
	ResolvedReasoningEffort            string
	ReasoningEffortResolved            bool
	Metadata                           map[string]string
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
	Kind string
	// ItemID is the Responses output-item ID the delta belongs to. The gateway
	// is the only component that assigns output-item IDs, so the HTTP layer
	// forwards this value rather than minting one of its own. It is required on
	// "delta" and "reasoning_delta" events.
	ItemID   string
	Delta    string
	Response *openai.Response
	Item     *openai.ResponseOutputItem
	Error    error
}
