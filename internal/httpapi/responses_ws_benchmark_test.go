package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func BenchmarkDecodeWebSocketResponseCreateFlat(b *testing.B) {
	raw := json.RawMessage(`{"type":"response.create","event_id":"evt","model":"gpt-5","input":"` + strings.Repeat("x", 64*1024) + `"}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for range b.N {
		if _, _, _, err := decodeWebSocketResponseCreate(raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeWebSocketResponseCreateNested(b *testing.B) {
	raw := json.RawMessage(`{"type":"response.create","event_id":"evt","response":{"model":"gpt-5","input":"` + strings.Repeat("x", 64*1024) + `"}}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for range b.N {
		if _, _, _, err := decodeWebSocketResponseCreate(raw); err != nil {
			b.Fatal(err)
		}
	}
}
