package sessionstore

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"
)

// ResponseRecord embeds the openai wire types directly. The gateway used to
// mirror them into sessionstore-local copies so that a wire change could not
// silently change the persisted format, but that mapping was hand-written and
// unchecked, so a new wire field was dropped rather than caught.
//
// These goldens replace it with a mechanism that actually holds: they pin the
// exact bytes writeJSON puts on disk for a record exercising every output item
// type, tool spec shape, tool output, loaded tool event and usage field. Any
// openai change that moves the persisted format fails here with a diff.
//
// The goldens are a contract, not a snapshot to be blindly refreshed. Changing
// them is changing the on-disk format; do it deliberately.
const goldenResponseRecordJSON = `{"version":3,"id":"resp_golden","sdk_session_id":"sess_golden","model":"gpt-5","instructions":"be brief","created_at":"2026-07-25T18:56:00Z","updated_at":"2026-07-25T18:57:30Z","status":"completed","stored":true,"deleted":false,"input_text":"find tools","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[{"type":"note","value":"x"}]}]},{"type":"message","id":"msg_2","status":"in_progress","role":"assistant","content":[]},{"type":"reasoning","status":"completed","encrypted_content":"cipher","id":"rs_1","summary":[{"type":"summary_text","text":"thought"}]},{"type":"reasoning","id":"rs_2","summary":[]},{"id":"fc_1","type":"function_call","status":"completed","namespace":"ns","call_id":"call_f","name":"lookup","arguments":"{\"q\":\"x\"}"},{"id":"ct_1","type":"custom_tool_call","status":"completed","call_id":"call_c","name":"apply_patch","input":"diff"},{"id":"ts_1","type":"tool_search_call","status":"completed","call_id":"call_s","name":"tool_search","execution":"client","output":{"ok":true},"arguments":{"query":"agents"}},{"id":"ts_2","type":"tool_search_call","call_id":"call_s2","arguments":"plain string arguments"}],"output_text":"hello","usage":{"input_tokens":11,"input_tokens_details":{"cached_tokens":3},"output_tokens":7,"output_tokens_details":{"reasoning_tokens":2},"total_tokens":18},"previous_response_id":"resp_prev","pending_batch_id":"batch_1","retained_path":"/retained/resp_golden","installed_tool_catalog":{"schema_version":1,"catalog_key":"catalog-key","tools":[{"type":"function","name":"lookup","namespace":"ns","description":"look things up","parameters":{"type":"object"},"execution":"server","strict":true,"defer_loading":false},{"type":"custom","name":"apply_patch","format":{"type":"grammar"}},{"type":"namespace","name":"multi_agent_v1","tools":[{"type":"function","name":"spawn_agent"}]},{"type":"tool_search","name":"tool_search","execution":"client"}]},"loaded_tool_events":[{"source_call_id":"call_s","response_id":"resp_golden","status":"completed","execution":"client","raw_tools":[{"name":"spawn_agent"}],"loaded_tools":[{"type":"namespace","name":"multi_agent_v1","tools":[{"type":"function","name":"spawn_agent"}]}]},{"source_call_id":"call_s2","response_id":"resp_golden"}],"tool_outputs":[{"type":"function_call_output","call_id":"call_f","name":"lookup","output":"done","status":"completed","execution":"server"},{"type":"tool_search_output","call_id":"call_s","tools":[{"name":"spawn_agent"}]}]}`

// goldenTombstoneRecordJSON pins the delete tombstone, whose semantics the
// retention path depends on: deleted is true and output serializes as null
// rather than being omitted.
const goldenTombstoneRecordJSON = `{"version":3,"id":"resp_gone","sdk_session_id":"","model":"","created_at":"2026-07-25T18:56:00Z","updated_at":"2026-07-25T18:57:30Z","status":"deleted","stored":false,"deleted":true,"output":null,"output_text":""}`

// goldenKnownEmptyCatalogJSON pins StoredToolCatalog.KnownEmpty, which only
// ever appears alongside an empty tool list and so cannot be covered by the
// catalog in goldenResponseRecordJSON.
const goldenKnownEmptyCatalogJSON = `{"schema_version":1,"catalog_key":"empty-key","tools":null,"known_empty":true}`

func goldenResponseRecord() ResponseRecord {
	created := time.Date(2026, 7, 25, 18, 56, 0, 0, time.UTC)
	updated := time.Date(2026, 7, 25, 18, 57, 30, 0, time.UTC)
	inputTokens, cachedTokens := int64(11), int64(3)
	outputTokens, reasoningTokens, totalTokens := int64(7), int64(2), int64(18)
	strict, deferLoading := true, false
	return ResponseRecord{
		Version:      ResponseRecordVersion,
		ID:           "resp_golden",
		SDKSessionID: "sess_golden",
		Model:        "gpt-5",
		Instructions: "be brief",
		CreatedAt:    created,
		UpdatedAt:    updated,
		Status:       "completed",
		Stored:       true,
		InputText:    "find tools",
		Output: []openai.ResponseOutputItem{
			{ID: "msg_1", Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: "hello", Annotations: []any{map[string]any{"type": "note", "value": "x"}}}}},
			{ID: "msg_2", Type: "message", Status: "in_progress", Role: "assistant"},
			{ID: "rs_1", Type: "reasoning", Status: "completed", Summary: []openai.ResponseReasoningSummary{{Type: "summary_text", Text: "thought"}}, EncryptedContent: "cipher"},
			{ID: "rs_2", Type: "reasoning"},
			{ID: "fc_1", Type: "function_call", Status: "completed", CallID: "call_f", Name: "lookup", Namespace: "ns", Arguments: `{"q":"x"}`},
			{ID: "ct_1", Type: "custom_tool_call", Status: "completed", CallID: "call_c", Name: "apply_patch", Input: "diff"},
			{ID: "ts_1", Type: "tool_search_call", Status: "completed", CallID: "call_s", Name: "tool_search", Execution: "client", ArgumentsJSON: json.RawMessage(`{"query":"agents"}`), Output: json.RawMessage(`{"ok":true}`)},
			{ID: "ts_2", Type: "tool_search_call", CallID: "call_s2", Arguments: "plain string arguments"},
		},
		OutputText: "hello",
		Usage: &openai.ResponseUsage{
			InputTokens:         &inputTokens,
			InputTokensDetails:  &openai.ResponseInputTokensDetails{CachedTokens: &cachedTokens},
			OutputTokens:        &outputTokens,
			OutputTokensDetails: &openai.ResponseOutputTokensDetails{ReasoningTokens: &reasoningTokens},
			TotalTokens:         &totalTokens,
		},
		PreviousResponseID: "resp_prev",
		PendingBatchID:     "batch_1",
		RetainedPath:       "/retained/resp_golden",
		InstalledToolCatalog: &openai.StoredToolCatalog{
			SchemaVersion: 1,
			CatalogKey:    "catalog-key",
			Tools: []openai.StoredToolSpec{
				{Type: openai.ToolKindFunction, Name: "lookup", Namespace: "ns", Description: "look things up", Parameters: json.RawMessage(`{"type":"object"}`), Execution: "server", Strict: &strict, DeferLoading: &deferLoading},
				{Type: openai.ToolKindCustom, Name: "apply_patch", Format: json.RawMessage(`{"type":"grammar"}`)},
				{Type: openai.ToolKindNamespace, Name: "multi_agent_v1", Tools: []openai.StoredToolSpec{{Type: openai.ToolKindFunction, Name: "spawn_agent"}}},
				{Type: openai.ToolKindToolSearch, Name: "tool_search", Execution: "client"},
			},
		},
		LoadedToolEvents: []openai.StoredLoadedToolEvent{
			{SourceCallID: "call_s", ResponseID: "resp_golden", Status: "completed", Execution: "client", RawTools: json.RawMessage(`[{"name":"spawn_agent"}]`), LoadedTools: []openai.StoredToolSpec{{Type: openai.ToolKindNamespace, Name: "multi_agent_v1", Tools: []openai.StoredToolSpec{{Type: openai.ToolKindFunction, Name: "spawn_agent"}}}}},
			{SourceCallID: "call_s2", ResponseID: "resp_golden"},
		},
		ToolOutputs: []openai.StoredToolOutput{
			{Type: "function_call_output", CallID: "call_f", Name: "lookup", Output: "done", Status: "completed", Execution: "server"},
			{Type: "tool_search_output", CallID: "call_s", Tools: json.RawMessage(`[{"name":"spawn_agent"}]`)},
		},
	}
}

// goldenResponseRecordAfterRoundTrip is goldenResponseRecord after a JSON
// round trip. ResponseOutputItem.MarshalJSON forces the arrays the Responses
// schema declares required, so a message with nil Content and a reasoning item
// with nil Summary are written as `[]` and decode back as empty-but-non-nil.
// That normalization is deliberate (see openai.ResponseOutputItem.MarshalJSON);
// spelling it out here keeps the round-trip assertions exact instead of
// loosening them.
func goldenResponseRecordAfterRoundTrip() ResponseRecord {
	record := goldenResponseRecord()
	record.Output[1].Content = []openai.ResponseText{}
	record.Output[3].Summary = []openai.ResponseReasoningSummary{}
	return record
}

func TestResponseRecordGoldenJSON(t *testing.T) {
	got, err := json.Marshal(goldenResponseRecord())
	if err != nil {
		t.Fatal(err)
	}
	assertGoldenJSON(t, got, goldenResponseRecordJSON)
}

func TestResponseRecordTombstoneGoldenJSON(t *testing.T) {
	record := ResponseRecord{
		Version:   ResponseRecordVersion,
		ID:        "resp_gone",
		CreatedAt: time.Date(2026, 7, 25, 18, 56, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 7, 25, 18, 57, 30, 0, time.UTC),
		Status:    "deleted",
		Deleted:   true,
	}
	got, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	assertGoldenJSON(t, got, goldenTombstoneRecordJSON)
}

func TestResponseRecordKnownEmptyCatalogGoldenJSON(t *testing.T) {
	got, err := json.Marshal(&openai.StoredToolCatalog{SchemaVersion: 1, CatalogKey: "empty-key", KnownEmpty: true})
	if err != nil {
		t.Fatal(err)
	}
	assertGoldenJSON(t, got, goldenKnownEmptyCatalogJSON)
}

// TestResponseRecordGoldenDecodesToRecord closes the loop: the pinned bytes
// must decode back into the record that produced them, so the golden cannot be
// satisfied by a format that persists fields the loader then drops.
func TestResponseRecordGoldenDecodesToRecord(t *testing.T) {
	var decoded ResponseRecord
	if err := json.Unmarshal([]byte(goldenResponseRecordJSON), &decoded); err != nil {
		t.Fatal(err)
	}
	want := goldenResponseRecordAfterRoundTrip()
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("golden JSON did not decode back into the record:\n%s", goldenRecordDiff(decoded, want))
	}
	reencoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	assertGoldenJSON(t, reencoded, goldenResponseRecordJSON)
}

// TestResponseRecordGoldenSurvivesStore verifies the golden describes what the
// store actually writes and reads, not just what json.Marshal does in isolation.
func TestResponseRecordGoldenSurvivesStore(t *testing.T) {
	store := New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	record := goldenResponseRecord()
	if err := store.SaveResponse(record); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadResponse(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	// SaveResponse stamps UpdatedAt; every payload field must be untouched.
	loaded.UpdatedAt = record.UpdatedAt
	if want := goldenResponseRecordAfterRoundTrip(); !reflect.DeepEqual(loaded, want) {
		t.Fatalf("store round trip changed the record:\n%s", goldenRecordDiff(loaded, want))
	}
	got, err := json.Marshal(loaded)
	if err != nil {
		t.Fatal(err)
	}
	assertGoldenJSON(t, got, goldenResponseRecordJSON)
}

func assertGoldenJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	if string(got) == want {
		return
	}
	t.Fatalf("persisted response record format changed.\n%s\n\nthe on-disk format is a contract: fix the openai type, or update the golden deliberately", goldenJSONDiff(got, []byte(want)))
}

// goldenRecordDiff renders two records as indented JSON so a round-trip failure
// is readable, rather than a pair of one-line %#v dumps.
func goldenRecordDiff(got, want ResponseRecord) string {
	gotJSON, err := json.Marshal(got)
	if err != nil {
		return err.Error()
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		return err.Error()
	}
	if bytes.Equal(gotJSON, wantJSON) {
		return "records differ only in Go-level nil versus empty slices; both encode to:\n" + string(goldenIndent(gotJSON))
	}
	return goldenJSONDiff(gotJSON, wantJSON)
}

// goldenJSONDiff reports the first differing byte with surrounding context and
// the indented forms, so a failure names the field that moved instead of
// dumping two unreadable single-line blobs.
func goldenJSONDiff(got, want []byte) string {
	offset := 0
	for offset < len(got) && offset < len(want) && got[offset] == want[offset] {
		offset++
	}
	var b bytes.Buffer
	b.WriteString("first difference at byte ")
	b.WriteString(itoa(offset))
	b.WriteString(":\n  got  ...")
	b.Write(goldenExcerpt(got, offset))
	b.WriteString("\n  want ...")
	b.Write(goldenExcerpt(want, offset))
	b.WriteString("\n\nfull got (indented):\n")
	b.Write(goldenIndent(got))
	b.WriteString("\n\nfull want (indented):\n")
	b.Write(goldenIndent(want))
	return b.String()
}

func goldenExcerpt(data []byte, offset int) []byte {
	start := offset - 40
	if start < 0 {
		start = 0
	}
	end := offset + 80
	if end > len(data) {
		end = len(data)
	}
	return data[start:end]
}

func goldenIndent(data []byte) []byte {
	var out bytes.Buffer
	if err := json.Indent(&out, data, "  ", "  "); err != nil {
		return data
	}
	return out.Bytes()
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	return string(digits[i:])
}
