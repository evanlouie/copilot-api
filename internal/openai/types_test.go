package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestChatCompletionChunkUsageSerialization(t *testing.T) {
	// Without include_usage, usage is omitted entirely when nil.
	b, err := json.Marshal(ChatCompletionChunk{ID: "c", Object: ObjectChatChunk})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"usage"`) {
		t.Fatalf("usage should be omitted without include_usage: %s", b)
	}

	// With include_usage and no usage, usage is present and explicitly null.
	b, err = json.Marshal(ChatCompletionChunk{ID: "c", Object: ObjectChatChunk, IncludeUsage: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"usage":null`) {
		t.Fatalf("usage should be null on non-terminal include_usage chunks: %s", b)
	}

	// With include_usage and a usage value, the value is serialized.
	total := int64(8)
	b, err = json.Marshal(ChatCompletionChunk{ID: "c", Object: ObjectChatChunk, IncludeUsage: true, Usage: &Usage{TotalTokens: &total}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"total_tokens":8`) {
		t.Fatalf("usage value should be serialized: %s", b)
	}

	// The internal include_usage flag is never serialized, and the embedded
	// alias keeps all other fields present (guards against field drift).
	if strings.Contains(string(b), "include_usage") || strings.Contains(string(b), "IncludeUsage") {
		t.Fatalf("internal include_usage flag leaked into JSON: %s", b)
	}
	if !strings.Contains(string(b), `"id":"c"`) || !strings.Contains(string(b), `"system_fingerprint"`) {
		t.Fatalf("embedded chunk fields missing from include_usage output: %s", b)
	}
}
