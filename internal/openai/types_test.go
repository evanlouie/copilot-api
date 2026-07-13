package openai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequestRawFieldsRetainOnlyValidationPresence(t *testing.T) {
	var chat ChatCompletionRequest
	if err := json.Unmarshal([]byte(`{"model":"gpt-test","messages":[{"role":"user","content":"large-known-field"}],"n":1}`), &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Raw) != 1 || string(chat.Raw["n"]) != "1" {
		t.Fatalf("chat.Raw = %#v, want only n presence", chat.Raw)
	}
	var responses ResponsesRequest
	if err := json.Unmarshal([]byte(`{"model":"gpt-test","input":"large-known-field","tools":[],"temperature":0.5}`), &responses); err != nil {
		t.Fatal(err)
	}
	if _, toolsPresent := responses.Raw["tools"]; len(responses.Raw) != 2 || !toolsPresent || string(responses.Raw["temperature"]) != "0.5" {
		t.Fatalf("responses.Raw = %#v, want only tools and temperature presence", responses.Raw)
	}
}

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
