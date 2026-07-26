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
	PromptTokens            int64               `json:"prompt_tokens"`
	CompletionTokens        int64               `json:"completion_tokens"`
	TotalTokens             int64               `json:"total_tokens"`
	PromptTokensDetails     *PromptTokenDetails `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *TokenDetails       `json:"completion_tokens_details,omitempty"`
}

// TokenDetails stays optional: unlike its Responses counterpart,
// completion_tokens_details is not a required member of the Chat usage object,
// and reasoning tokens are genuinely absent for non-reasoning models.
type TokenDetails struct {
	ReasoningTokens *int64 `json:"reasoning_tokens,omitempty"`
}

// PromptTokenDetails is Chat's prompt_tokens_details. Only cached_tokens is
// modelled: OpenAI also declares audio_tokens there, which this proxy has no
// source for and therefore does not claim a value for.
type PromptTokenDetails struct {
	CachedTokens *int64 `json:"cached_tokens,omitempty"`

	// CacheWriteTokens never reaches the Chat wire - Chat has no such member -
	// but the Responses schema requires input_tokens_details.cache_write_tokens,
	// and *Usage is the carrier every producer fills before either surface maps
	// from it. Keeping it here rather than inventing a parallel usage type is
	// the smaller lie; json:"-" is what makes it inert for Chat.
	CacheWriteTokens *int64 `json:"-"`
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
	// Both members are required by the Responses schema - see
	// openai-python's types/responses/response_usage.py, where InputTokensDetails
	// declares cache_write_tokens and cached_tokens as plain ints - so both are
	// always emitted.
	CacheWriteTokens int64 `json:"cache_write_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
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
	if usage.CompletionTokensDetails != nil && usage.CompletionTokensDetails.ReasoningTokens != nil {
		out.OutputTokensDetails.ReasoningTokens = *usage.CompletionTokensDetails.ReasoningTokens
	}
	if d := usage.PromptTokensDetails; d != nil {
		if d.CachedTokens != nil {
			out.InputTokensDetails.CachedTokens = *d.CachedTokens
		}
		if d.CacheWriteTokens != nil {
			out.InputTokensDetails.CacheWriteTokens = *d.CacheWriteTokens
		}
	}
	out.Normalize()
	return out
}

// Normalize repairs a usage object whose total contradicts its own addends.
//
// It exists for records read back from disk. Producers derive the total, but a
// record written by an older build - when the counters were pointers with
// omitempty - decodes its absent fields as 0, so a stored
// {"input_tokens":11} comes back as input_tokens 11 next to total_tokens 0.
// That is not "unreported", it is arithmetically false, and it is exactly the
// null-arithmetic class of breakage the required-integer contract exists to
// prevent. A zero total beside a non-zero counter can only mean the source
// never carried one.
func (u *ResponseUsage) Normalize() {
	if u == nil {
		return
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.InputTokens + u.OutputTokens
	}
}
