package openai

import (
	"regexp"
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

// ResponseIDPrefix is the prefix every response id this proxy mints carries.
// Every mint goes through NewID(ResponseIDPrefix); see the call sites in
// internal/httpapi and internal/copilotgw.
const ResponseIDPrefix = "resp_"

// responseIDRE is the grammar NewID(ResponseIDPrefix) can produce: the fixed
// prefix followed by a UUID. The body is matched as a character class rather
// than parsed as a UUID so a future change to the id body does not silently
// start rejecting live ids, but the class is deliberately the URL-safe one —
// no dot and no slash — which makes an id that satisfies it incapable of
// naming anything but itself on disk, independently of what sessionstore does
// with it.
var responseIDRE = regexp.MustCompile(`^` + ResponseIDPrefix + `[A-Za-z0-9_-]{1,128}$`)

// ValidResponseID reports whether id has the shape this proxy mints. It is the
// grammar the transport validates a path segment against; nothing else can
// name a stored response.
func ValidResponseID(id string) bool { return responseIDRE.MatchString(id) }

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

// Usage is the Chat Completions usage object. OpenAI declares prompt_tokens,
// completion_tokens and total_tokens as required integers, so the counters are
// plain values with no omitempty: whenever a usage object is on the wire it
// carries all three. Optionality lives one level up, in the *Usage the
// containing payload holds — an absent usage object is legal, a half-populated
// one is not, because clients deserialize the counters into non-optional
// integers and cost middleware adds them without a nil check.
//
// Producers must therefore decide all-or-nothing: emit nil, or emit an object
// with every counter set. See copilotgw.usageFromSDK for the gate.
type Usage struct {
	PromptTokens            int64         `json:"prompt_tokens"`
	CompletionTokens        int64         `json:"completion_tokens"`
	TotalTokens             int64         `json:"total_tokens"`
	CompletionTokensDetails *TokenDetails `json:"completion_tokens_details,omitempty"`
}

// TokenDetails stays optional: unlike its Responses counterpart,
// completion_tokens_details is not a required member of the Chat usage object,
// and reasoning tokens are genuinely absent for non-reasoning models.
type TokenDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
}

// ResponseUsage is the Responses usage object. The Responses schema declares
// every member required, including both details objects, so nothing here is
// optional or omitempty; the details objects are values rather than pointers so
// they cannot structurally go missing.
type ResponseUsage struct {
	InputTokens         int64                       `json:"input_tokens"`
	InputTokensDetails  ResponseInputTokensDetails  `json:"input_tokens_details"`
	OutputTokens        int64                       `json:"output_tokens"`
	OutputTokensDetails ResponseOutputTokensDetails `json:"output_tokens_details"`
	TotalTokens         int64                       `json:"total_tokens"`
}

type ResponseInputTokensDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

type ResponseOutputTokensDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

// NewResponseUsage renames the Chat counters onto the Responses object. The
// all-or-nothing decision was already made by whoever produced the *Usage, so
// this only propagates absence; a non-nil input always yields a complete object.
func NewResponseUsage(usage *Usage) *ResponseUsage {
	if usage == nil {
		return nil
	}
	out := &ResponseUsage{InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens}
	if out.TotalTokens == 0 {
		// A zero total next to non-zero counters means the source never carried
		// one, not that the turn consumed nothing; derive it rather than ship a
		// total that contradicts its own addends.
		out.TotalTokens = out.InputTokens + out.OutputTokens
	}
	if usage.CompletionTokensDetails != nil && usage.CompletionTokensDetails.ReasoningTokens != nil {
		out.OutputTokensDetails.ReasoningTokens = *usage.CompletionTokensDetails.ReasoningTokens
	}
	return out
}
