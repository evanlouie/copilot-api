package copilotgw

import (
	"encoding/json"
	"testing"

	"github.com/evanlouie/copilot-api/internal/openai"
	copilot "github.com/github/copilot-sdk/go"
)

func i64(v int64) *int64 { return &v }

// TestUsageCarriesPromptCacheAccounting pins that the SDK's prompt-cache
// counters reach the wire.
//
// input_tokens_details is a required member of the Responses usage object, so
// once the counters stopped being optional this proxy started emitting
// cached_tokens on every turn whether or not it had a value for it. Dropping
// the SDK's numbers therefore stopped being an omission and became a positive
// claim of zero cache reuse - a wrong number rather than a missing one, which
// clients act on: Codex maps cached_tokens straight into its cost display.
func TestUsageCarriesPromptCacheAccounting(t *testing.T) {
	t.Parallel()
	usage := usageFromSDK(&copilot.AssistantUsageData{
		InputTokens:      i64(100),
		OutputTokens:     i64(20),
		CacheReadTokens:  i64(64),
		CacheWriteTokens: i64(8),
	})
	if usage == nil {
		t.Fatal("usageFromSDK dropped a usage event that reported token counts")
	}
	if usage.PromptTokensDetails == nil {
		t.Fatal("Chat usage lost the prompt-cache detail the SDK reported")
	}
	if got := usage.PromptTokensDetails.CachedTokens; got == nil || *got != 64 {
		t.Fatalf("chat cached_tokens = %v, want 64", got)
	}

	resp := openai.NewResponseUsage(usage)
	if resp.InputTokensDetails.CachedTokens != 64 {
		t.Fatalf("responses cached_tokens = %d, want 64", resp.InputTokensDetails.CachedTokens)
	}
	if resp.InputTokensDetails.CacheWriteTokens != 8 {
		t.Fatalf("responses cache_write_tokens = %d, want 8", resp.InputTokensDetails.CacheWriteTokens)
	}

	// cache_write_tokens is required by the Responses schema but has no Chat
	// member, so it must be carried without ever reaching the Chat wire.
	encoded, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	if err := json.Unmarshal(encoded, &chat); err != nil {
		t.Fatal(err)
	}
	details, ok := chat["prompt_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("chat usage is missing prompt_tokens_details: %s", encoded)
	}
	if _, leaked := details["cache_write_tokens"]; leaked {
		t.Fatalf("cache_write_tokens leaked onto the Chat wire: %s", encoded)
	}
}

// TestUsageOmitsPromptCacheDetailWhenTheSDKReportsNone keeps the fix honest in
// the other direction: a turn the SDK said nothing about must not gain a
// fabricated Chat detail object.
func TestUsageOmitsPromptCacheDetailWhenTheSDKReportsNone(t *testing.T) {
	t.Parallel()
	usage := usageFromSDK(&copilot.AssistantUsageData{InputTokens: i64(5), OutputTokens: i64(5)})
	if usage == nil {
		t.Fatal("usageFromSDK dropped a usage event that reported token counts")
	}
	if usage.PromptTokensDetails != nil {
		t.Fatalf("invented prompt_tokens_details from nothing: %#v", usage.PromptTokensDetails)
	}
	// The Responses object still carries the member, because its schema requires
	// it - but only there, where absence is not expressible.
	if got := openai.NewResponseUsage(usage); got.InputTokensDetails.CachedTokens != 0 {
		t.Fatalf("responses cached_tokens = %d, want 0", got.InputTokensDetails.CachedTokens)
	}
}

// TestStoredUsageTotalIsRepairedOnRead covers records written before the
// counters became required integers. Those stored the fields as pointers with
// omitempty, so an absent counter decodes as 0 and a stored {"input_tokens":11}
// reads back as 11 input tokens against a 0 total - not "unreported", but
// arithmetic that contradicts itself.
func TestStoredUsageTotalIsRepairedOnRead(t *testing.T) {
	t.Parallel()
	var usage *openai.ResponseUsage
	if err := json.Unmarshal([]byte(`{"input_tokens":11}`), &usage); err != nil {
		t.Fatal(err)
	}
	if usage.TotalTokens != 0 {
		t.Fatalf("precondition: legacy record decoded with total_tokens = %d", usage.TotalTokens)
	}
	usage.Normalize()
	if usage.TotalTokens != 11 {
		t.Fatalf("total_tokens = %d, want 11 after repair", usage.TotalTokens)
	}

	// A turn that genuinely consumed nothing stays at zero rather than being
	// invented into something.
	zero := &openai.ResponseUsage{}
	zero.Normalize()
	if zero.TotalTokens != 0 {
		t.Fatalf("total_tokens = %d, want 0 for an empty usage object", zero.TotalTokens)
	}
}
