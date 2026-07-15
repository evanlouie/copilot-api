package httpapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestParseResponsesInputJoinsManyUserMessages(t *testing.T) {
	raw := json.RawMessage(`[{"type":"message","role":"user","content":"one"},{"type":"message","role":"user","content":"two"}]`)
	prompt, outputs, instructions, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(outputs) != 0 || instructions != "" || prompt.Text != "one\ntwo" {
		t.Fatalf("parseResponsesInput = (%q, %#v, %q), want joined user text", prompt.Text, outputs, instructions)
	}
}

func BenchmarkParseResponsesInputMixedContinuation(b *testing.B) {
	items := make([]map[string]any, 0, 2_002)
	for i := 0; i < 2_000; i++ {
		items = append(items, map[string]any{"type": "message", "role": "user", "content": fmt.Sprintf("message-%04d-%s", i, strings.Repeat("x", 64))})
	}
	items = append(items,
		map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "done"},
		map[string]any{"type": "message", "role": "user", "content": "summarize"},
	)
	raw, err := json.Marshal(items)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for range b.N {
		if _, err := parseResponsesInputOnce(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseResponsesInputManyMessages(b *testing.B) {
	items := make([]map[string]any, 2_000)
	for i := range items {
		items[i] = map[string]any{"type": "message", "role": "user", "content": fmt.Sprintf("message-%04d-%s", i, strings.Repeat("x", 64))}
	}
	raw, err := json.Marshal(items)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	b.ResetTimer()
	for range b.N {
		if _, _, _, err := parseResponsesInput(raw); err != nil {
			b.Fatal(err)
		}
	}
}
