package copilotgw

import (
	"encoding/json"
	"testing"

	"github.com/evanlouie/copilot-api/internal/openai"
	copilot "github.com/github/copilot-sdk/go"
)

// The usage object is all-or-nothing on both wires. OpenAI declares
// prompt_tokens/completion_tokens/total_tokens (Chat) and
// input_tokens/output_tokens/total_tokens plus both details objects (Responses)
// as always present, and clients deserialize them into non-optional integers, so
// a usage object that carries only the counters the SDK happened to report is a
// serde failure rather than a smaller payload. The SDK reports input and output
// tokens independently, so "output tokens only" is a reachable upstream shape.
func TestUsageFromSDKEmitsCompleteUsageObject(t *testing.T) {
	output := int64(12)
	usage := usageFromSDK(&copilot.AssistantUsageData{OutputTokens: &output})
	if usage == nil {
		t.Fatal("usageFromSDK dropped a usage event that reported output tokens")
	}

	chat := marshalUsageObject(t, usage)
	assertUsageKeys(t, "chat", chat, "prompt_tokens", "completion_tokens", "total_tokens")
	assertUsageNumber(t, "chat", chat, "prompt_tokens", 0)
	assertUsageNumber(t, "chat", chat, "completion_tokens", 12)
	assertUsageNumber(t, "chat", chat, "total_tokens", 12)

	responses := marshalUsageObject(t, openai.NewResponseUsage(usage))
	assertUsageKeys(t, "responses", responses, "input_tokens", "input_tokens_details", "output_tokens", "output_tokens_details", "total_tokens")
	assertUsageNumber(t, "responses", responses, "input_tokens", 0)
	assertUsageNumber(t, "responses", responses, "output_tokens", 12)
	assertUsageNumber(t, "responses", responses, "total_tokens", 12)
	assertUsageKeys(t, "responses.input_tokens_details", nestedUsageObject(t, responses, "input_tokens_details"), "cached_tokens")
	assertUsageKeys(t, "responses.output_tokens_details", nestedUsageObject(t, responses, "output_tokens_details"), "reasoning_tokens")
}

// A usage event with no token counts at all carries nothing a client can use,
// so the gate is absence of the whole object rather than a partial one.
func TestUsageFromSDKDropsCountlessUsage(t *testing.T) {
	reasoning := int64(4)
	if usage := usageFromSDK(&copilot.AssistantUsageData{ReasoningTokens: &reasoning}); usage != nil {
		t.Fatalf("usageFromSDK = %#v, want nil for a usage event with no token counts", usage)
	}
	if usage := openai.NewResponseUsage(nil); usage != nil {
		t.Fatalf("NewResponseUsage(nil) = %#v, want nil", usage)
	}
}

func marshalUsageObject(t *testing.T, v any) map[string]json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("usage did not serialize as an object: %s: %v", raw, err)
	}
	return fields
}

func nestedUsageObject(t *testing.T, fields map[string]json.RawMessage, key string) map[string]json.RawMessage {
	t.Helper()
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(fields[key], &nested); err != nil {
		t.Fatalf("%s did not serialize as an object: %s: %v", key, fields[key], err)
	}
	return nested
}

func assertUsageKeys(t *testing.T, wire string, fields map[string]json.RawMessage, keys ...string) {
	t.Helper()
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			t.Errorf("%s usage object is missing required key %q; got %v", wire, key, usageKeys(fields))
		}
	}
}

func assertUsageNumber(t *testing.T, wire string, fields map[string]json.RawMessage, key string, want int64) {
	t.Helper()
	raw, ok := fields[key]
	if !ok {
		return // assertUsageKeys already reported the absence.
	}
	var got int64
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Errorf("%s usage %s is not an integer: %s: %v", wire, key, raw, err)
		return
	}
	if got != want {
		t.Errorf("%s usage %s = %d, want %d", wire, key, got, want)
	}
}

func usageKeys(fields map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	return keys
}
