package sessionstore

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
)

// benchmarkResponseRecord builds a record of roughly outputBytes of persisted
// payload, shaped like a long agent turn: a large output_text plus a run of
// output items and tool outputs. Turns are capped at 32 MiB, so the record a
// busy conversation re-saves on every turn is large by design.
func benchmarkResponseRecord(id, sessionID string, outputBytes int, items int) ResponseRecord {
	record := ResponseRecord{
		ID:           id,
		SDKSessionID: sessionID,
		Model:        "gpt-5",
		Instructions: "be brief",
		Status:       "completed",
		Stored:       true,
		InputText:    strings.Repeat("q", 4096),
		OutputText:   strings.Repeat("x", outputBytes),
		Usage:        &openai.ResponseUsage{InputTokens: 11, OutputTokens: 7, TotalTokens: 18},
	}
	for i := range items {
		record.Output = append(record.Output,
			openai.ResponseOutputItem{ID: fmt.Sprintf("msg_%d", i), Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: strings.Repeat("y", 256)}}},
			openai.ResponseOutputItem{ID: fmt.Sprintf("fc_%d", i), Type: "function_call", Status: "completed", CallID: fmt.Sprintf("call_%d", i), Name: "lookup", Arguments: `{"q":"x"}`},
		)
		record.ToolOutputs = append(record.ToolOutputs, toolcatalog.StoredToolOutput{Type: "function_call_output", CallID: fmt.Sprintf("call_%d", i), Name: "lookup", Output: strings.Repeat("z", 256), Status: "completed"})
	}
	return record
}

func benchmarkStore(b *testing.B) *Store {
	b.Helper()
	store := New(b.TempDir(), b.TempDir())
	if err := store.Ensure(); err != nil {
		b.Fatal(err)
	}
	return store
}

// BenchmarkSaveResponseLargeRecord is the request hot path: a turn re-saves the
// same response id as the conversation grows, so every save observes the
// previous record already on disk.
func BenchmarkSaveResponseLargeRecord(b *testing.B) {
	store := benchmarkStore(b)
	record := benchmarkResponseRecord("resp_bench", "sdk_bench", 4<<20, 64)
	encoded, err := json.Marshal(record)
	if err != nil {
		b.Fatal(err)
	}
	if err := store.SaveResponse(record); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(encoded)))
	b.ResetTimer()
	for range b.N {
		if err := store.SaveResponse(record); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSaveResponseSmallRecord isolates the fixed overhead of a save so the
// large-record numbers can be read as "cost attributable to record size".
func BenchmarkSaveResponseSmallRecord(b *testing.B) {
	store := benchmarkStore(b)
	record := benchmarkResponseRecord("resp_small", "sdk_bench", 256, 1)
	if err := store.SaveResponse(record); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := store.SaveResponse(record); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSaveResponseFirstWrite covers the path where nothing is on disk yet,
// which must stay a single write with no read.
func BenchmarkSaveResponseFirstWrite(b *testing.B) {
	store := benchmarkStore(b)
	records := make([]ResponseRecord, 64)
	for i := range records {
		records[i] = benchmarkResponseRecord(fmt.Sprintf("resp_new_%d", i), "sdk_bench", 256, 1)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		record := records[i%len(records)]
		record.ID = fmt.Sprintf("%s_%d", record.ID, i)
		if err := store.SaveResponse(record); err != nil {
			b.Fatal(err)
		}
	}
}
