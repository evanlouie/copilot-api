package httpapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// benchmarkDecodeWebSocketResponseCreate mirrors the live read loop in
// (*Server).responsesWebSocket: unmarshal the envelope into a field map, then
// decode that map. Benchmarking anything else would measure a path no request
// ever takes.
func benchmarkDecodeWebSocketResponseCreate(b *testing.B, raw json.RawMessage) {
	b.Helper()
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for range b.N {
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal(raw, &fields); err != nil {
			b.Fatal(err)
		}
		if _, _, _, err := decodeWebSocketResponseCreateFields(fields); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeWebSocketResponseCreateFlat(b *testing.B) {
	benchmarkDecodeWebSocketResponseCreate(b, json.RawMessage(`{"type":"response.create","event_id":"evt","model":"gpt-5","input":"`+strings.Repeat("x", 64*1024)+`"}`))
}

func BenchmarkDecodeWebSocketResponseCreateNested(b *testing.B) {
	benchmarkDecodeWebSocketResponseCreate(b, json.RawMessage(`{"type":"response.create","event_id":"evt","response":{"model":"gpt-5","input":"`+strings.Repeat("x", 64*1024)+`"}}`))
}
