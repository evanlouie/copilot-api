package copilotgw

import (
	"testing"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

func TestModelMetadataIncludesTokenLimits(t *testing.T) {
	contextWindow := int64(200000)
	prompt := int64(180000)
	output := int64(8192)

	meta := modelMetadata("GPT 5", true, true, &TokenLimits{
		MaxContextWindowTokens: &contextWindow,
		MaxPromptTokens:        &prompt,
		MaxOutputTokens:        &output,
	}, nil)

	if got := meta["max_context_window_tokens"]; got != contextWindow {
		t.Fatalf("metadata max_context_window_tokens = %#v, want %d", got, contextWindow)
	}
	if got := meta["max_prompt_tokens"]; got != prompt {
		t.Fatalf("metadata max_prompt_tokens = %#v, want %d", got, prompt)
	}
	if got := meta["max_output_tokens"]; got != output {
		t.Fatalf("metadata max_output_tokens = %#v, want %d", got, output)
	}

	capabilities, ok := meta["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("metadata capabilities = %#v, want object", meta["capabilities"])
	}
	limits, ok := capabilities["limits"].(map[string]any)
	if !ok {
		t.Fatalf("metadata capabilities.limits = %#v, want object", capabilities["limits"])
	}
	if got := limits["max_context_window_tokens"]; got != contextWindow {
		t.Fatalf("metadata capabilities.limits.max_context_window_tokens = %#v, want %d", got, contextWindow)
	}
}

func TestRPCTokenLimits(t *testing.T) {
	contextWindow := int64(128000)
	prompt := int64(120000)
	output := int64(4096)

	limits := rpcTokenLimits(&rpc.ModelCapabilitiesLimits{
		MaxContextWindowTokens: &contextWindow,
		MaxPromptTokens:        &prompt,
		MaxOutputTokens:        &output,
	})
	if limits == nil {
		t.Fatal("expected token limits")
	}
	if *limits.MaxContextWindowTokens != contextWindow {
		t.Fatalf("MaxContextWindowTokens = %d, want %d", *limits.MaxContextWindowTokens, contextWindow)
	}
	if *limits.MaxPromptTokens != prompt {
		t.Fatalf("MaxPromptTokens = %d, want %d", *limits.MaxPromptTokens, prompt)
	}
	if *limits.MaxOutputTokens != output {
		t.Fatalf("MaxOutputTokens = %d, want %d", *limits.MaxOutputTokens, output)
	}
}

func TestSDKTokenLimits(t *testing.T) {
	prompt := 120000

	limits := sdkTokenLimits(copilot.ModelLimits{
		MaxContextWindowTokens: 128000,
		MaxPromptTokens:        &prompt,
	})
	if limits == nil {
		t.Fatal("expected token limits")
	}
	if *limits.MaxContextWindowTokens != 128000 {
		t.Fatalf("MaxContextWindowTokens = %d, want 128000", *limits.MaxContextWindowTokens)
	}
	if *limits.MaxPromptTokens != int64(prompt) {
		t.Fatalf("MaxPromptTokens = %d, want %d", *limits.MaxPromptTokens, prompt)
	}
	if limits.MaxOutputTokens != nil {
		t.Fatalf("MaxOutputTokens = %d, want nil", *limits.MaxOutputTokens)
	}
}
