package copilotgw

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
)

func TestPersistenceMappingsRoundTripAllResponseVariants(t *testing.T) {
	inputTokens, cachedTokens := int64(11), int64(3)
	outputTokens, reasoningTokens, totalTokens := int64(7), int64(2), int64(18)
	outputs := []openai.ResponseOutputItem{
		{ID: "msg", Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: "hello", Annotations: []any{map[string]any{"type": "note", "value": "x"}}}}},
		{ID: "rs", Type: "reasoning", Status: "completed", Summary: []openai.ResponseReasoningSummary{{Type: "summary_text", Text: "thought"}}, EncryptedContent: "encrypted"},
		{ID: "fc", Type: "function_call", Status: "completed", CallID: "call_f", Name: "lookup", Namespace: "ns", Arguments: `{"q":"x"}`},
		{ID: "ct", Type: "custom_tool_call", Status: "completed", CallID: "call_c", Name: "patch", Input: "diff"},
		{ID: "ts", Type: "tool_search_call", Status: "completed", CallID: "call_s", Execution: "client", ArgumentsJSON: json.RawMessage(`{"query":"tools"}`), Output: json.RawMessage(`{"ok":true}`)},
	}
	usage := &openai.ResponseUsage{
		InputTokens: &inputTokens, InputTokensDetails: &openai.ResponseInputTokensDetails{CachedTokens: &cachedTokens},
		OutputTokens: &outputTokens, OutputTokensDetails: &openai.ResponseOutputTokensDetails{ReasoningTokens: &reasoningTokens}, TotalTokens: &totalTokens,
	}
	strict := true
	catalog := &openai.StoredToolCatalog{SchemaVersion: 1, CatalogKey: "key", Tools: []openai.StoredToolSpec{{Type: openai.ToolKindFunction, Name: "lookup", Parameters: json.RawMessage(`{"type":"object"}`), Strict: &strict}}}
	events := []openai.StoredLoadedToolEvent{{SourceCallID: "call_s", ResponseID: "resp", RawTools: json.RawMessage(`[]`), LoadedTools: catalog.Tools}}
	toolOutputs := []openai.StoredToolOutput{{Type: "function_call_output", CallID: "call_f", Output: "done", Tools: json.RawMessage(`[]`)}}

	store := sessionstore.New(t.TempDir(), t.TempDir(), t.TempDir())
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	record := sessionstore.ResponseRecord{
		ID: "resp", Stored: true, Output: storeOutputItems(outputs), Usage: storeUsage(usage),
		InstalledToolCatalog: storeToolCatalog(catalog), LoadedToolEvents: storeLoadedToolEvents(events), ToolOutputs: storeToolOutputs(toolOutputs),
	}
	if err := store.SaveResponse(record); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadResponse("resp")
	if err != nil {
		t.Fatal(err)
	}
	if got := wireOutputItems(loaded.Output); !reflect.DeepEqual(got, outputs) {
		t.Fatalf("output round-trip mismatch:\n got %#v\nwant %#v", got, outputs)
	}
	if got := wireUsage(loaded.Usage); !reflect.DeepEqual(got, usage) {
		t.Fatalf("usage round-trip mismatch: got %#v want %#v", got, usage)
	}
	if got := wireToolCatalog(loaded.InstalledToolCatalog); !reflect.DeepEqual(got, catalog) {
		t.Fatalf("catalog round-trip mismatch: got %#v want %#v", got, catalog)
	}
	if got := wireLoadedToolEvents(loaded.LoadedToolEvents); !reflect.DeepEqual(got, events) {
		t.Fatalf("events round-trip mismatch: got %#v want %#v", got, events)
	}
	if got := wireToolOutputs(loaded.ToolOutputs); !reflect.DeepEqual(got, toolOutputs) {
		t.Fatalf("tool outputs round-trip mismatch: got %#v want %#v", got, toolOutputs)
	}
}
