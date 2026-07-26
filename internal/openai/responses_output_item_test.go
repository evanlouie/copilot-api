package openai

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestResponseOutputItemGoldenJSON pins the exact serialization of every
// Responses output item variant.
//
// The OpenAI Responses schema declares `id`/`content`/`role`/`status`/`type`
// required and non-nullable on a `message` item and `id`/`summary`/`type` on a
// `reasoning` item. The streaming path opens both with an empty array, and SDK
// stream accumulators append into those arrays on the first content/summary
// part, so a dropped key (Go's omitempty drops any zero-length slice) becomes
// None client-side and the append raises. These goldens fail loudly if the
// keys ever go missing again.
func TestResponseOutputItemGoldenJSON(t *testing.T) {
	tests := []struct {
		name string
		item ResponseOutputItem
		want string
	}{
		{
			name: "message with empty content",
			item: ResponseOutputItem{ID: "msg_1", Type: "message", Status: "in_progress", Role: "assistant", Content: []ResponseText{}},
			want: `{"type":"message","id":"msg_1","status":"in_progress","role":"assistant","content":[]}`,
		},
		{
			name: "message with nil content",
			item: ResponseOutputItem{ID: "msg_1", Type: "message", Status: "in_progress", Role: "assistant"},
			want: `{"type":"message","id":"msg_1","status":"in_progress","role":"assistant","content":[]}`,
		},
		{
			name: "message with content",
			item: ResponseOutputItem{ID: "msg_1", Type: "message", Status: "completed", Role: "assistant", Content: []ResponseText{{Type: "output_text", Text: "hello"}}},
			want: `{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[]}]}`,
		},
		{
			name: "reasoning with empty summary",
			item: ResponseOutputItem{ID: "rs_1", Type: "reasoning", Status: "in_progress", Summary: []ResponseReasoningSummary{}},
			want: `{"type":"reasoning","status":"in_progress","id":"rs_1","summary":[]}`,
		},
		{
			name: "reasoning with nil summary and encrypted content",
			item: ResponseOutputItem{ID: "rs_1", Type: "reasoning", Status: "completed", EncryptedContent: "blob"},
			want: `{"type":"reasoning","status":"completed","encrypted_content":"blob","id":"rs_1","summary":[]}`,
		},
		{
			name: "reasoning with summary",
			item: ResponseOutputItem{ID: "rs_1", Type: "reasoning", Status: "completed", Summary: []ResponseReasoningSummary{{Type: "summary_text", Text: "thinking"}}},
			want: `{"type":"reasoning","status":"completed","id":"rs_1","summary":[{"type":"summary_text","text":"thinking"}]}`,
		},
		{
			name: "function call",
			item: ResponseOutputItem{ID: "fc_1", Type: "function_call", Status: "completed", CallID: "call_1", Name: "lookup", Arguments: `{"q":"alpha"}`},
			want: `{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"alpha\"}"}`,
		},
		{
			name: "function call without arguments still carries the required keys",
			item: ResponseOutputItem{ID: "fc_1", Type: "function_call", Status: "completed", CallID: "call_1", Name: "ping"},
			want: `{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"ping","arguments":""}`,
		},
		{
			name: "namespaced function call",
			item: ResponseOutputItem{ID: "fc_1", Type: "function_call", Status: "completed", CallID: "call_1", Namespace: "mcp__grep_app", Name: "searchGitHub", Arguments: `{}`},
			want: `{"id":"fc_1","type":"function_call","status":"completed","namespace":"mcp__grep_app","call_id":"call_1","name":"searchGitHub","arguments":"{}"}`,
		},
		{
			name: "custom tool call",
			item: ResponseOutputItem{ID: "ctc_1", Type: "custom_tool_call", Status: "completed", CallID: "call_2", Name: "apply_patch", Input: "*** Begin Patch"},
			want: `{"id":"ctc_1","type":"custom_tool_call","status":"completed","call_id":"call_2","name":"apply_patch","input":"*** Begin Patch"}`,
		},
		{
			name: "custom tool call with empty input still carries the required keys",
			item: ResponseOutputItem{ID: "ctc_1", Type: "custom_tool_call", Status: "completed", CallID: "call_2", Name: "apply_patch"},
			want: `{"id":"ctc_1","type":"custom_tool_call","status":"completed","call_id":"call_2","name":"apply_patch","input":""}`,
		},
		{
			name: "tool search call with raw arguments",
			item: ResponseOutputItem{ID: "tsc_1", Type: "tool_search_call", Status: "completed", CallID: "call_3", Execution: "client", ArgumentsJSON: json.RawMessage(`{"query":"grep"}`)},
			want: `{"id":"tsc_1","type":"tool_search_call","status":"completed","call_id":"call_3","execution":"client","arguments":{"query":"grep"}}`,
		},
		{
			name: "tool search call with string arguments",
			item: ResponseOutputItem{ID: "tsc_1", Type: "tool_search_call", Status: "completed", CallID: "call_3", Execution: "client", Arguments: `{"query":"grep"}`},
			want: `{"id":"tsc_1","type":"tool_search_call","status":"completed","call_id":"call_3","arguments":"{\"query\":\"grep\"}","execution":"client"}`,
		},
		{
			name: "tool search call without arguments",
			item: ResponseOutputItem{ID: "tsc_1", Type: "tool_search_call", Status: "in_progress", CallID: "call_3", Execution: "client"},
			want: `{"id":"tsc_1","type":"tool_search_call","status":"in_progress","call_id":"call_3","execution":"client"}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.item)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("marshal =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

// TestResponseOutputItemOmitsForeignRequiredFields guards the other half of the
// contract: forcing message/reasoning keys must not leak them onto tool call
// items, where OpenAI does not declare them and `"content": null` would be its
// own compatibility problem.
func TestResponseOutputItemOmitsForeignRequiredFields(t *testing.T) {
	items := []ResponseOutputItem{
		{ID: "fc_1", Type: "function_call", Status: "completed", CallID: "call_1", Name: "lookup", Arguments: "{}"},
		{ID: "ctc_1", Type: "custom_tool_call", Status: "completed", CallID: "call_2", Name: "apply_patch", Input: "diff"},
		{ID: "tsc_1", Type: "tool_search_call", Status: "completed", CallID: "call_3", Execution: "client", ArgumentsJSON: json.RawMessage(`{"query":"grep"}`)},
	}
	for _, item := range items {
		t.Run(item.Type, func(t *testing.T) {
			b, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{`"content"`, `"summary"`, `"role"`} {
				if strings.Contains(string(b), forbidden) {
					t.Fatalf("%s item must not carry %s: %s", item.Type, forbidden, b)
				}
			}
		})
	}
}

// TestResponseOutputItemRoundTrip confirms the forced keys stay decode-clean,
// including the string-vs-object arguments split the marshaller special-cases.
func TestResponseOutputItemRoundTrip(t *testing.T) {
	items := []ResponseOutputItem{
		{ID: "msg_1", Type: "message", Status: "completed", Role: "assistant", Content: []ResponseText{{Type: "output_text", Text: "hi", Annotations: []any{}}}},
		{ID: "rs_1", Type: "reasoning", Status: "completed", Summary: []ResponseReasoningSummary{{Type: "summary_text", Text: "thinking"}}},
		{ID: "fc_1", Type: "function_call", Status: "completed", CallID: "call_1", Name: "lookup", Arguments: `{"q":"alpha"}`},
		{ID: "ctc_1", Type: "custom_tool_call", Status: "completed", CallID: "call_2", Name: "apply_patch", Input: "diff"},
		{ID: "tsc_1", Type: "tool_search_call", Status: "completed", CallID: "call_3", Execution: "client", ArgumentsJSON: json.RawMessage(`{"query":"grep"}`)},
	}
	for _, item := range items {
		t.Run(item.Type, func(t *testing.T) {
			b, err := json.Marshal(item)
			if err != nil {
				t.Fatal(err)
			}
			var got ResponseOutputItem
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, item) {
				t.Fatalf("round trip = %#v, want %#v (wire: %s)", got, item, b)
			}
		})
	}
}

// TestResponseOutputItemEmptyArraysSurviveResponseEnvelope checks the fix holds
// through the enclosing Response and stream-event envelopes, which is how
// clients actually see these items.
func TestResponseOutputItemEmptyArraysSurviveResponseEnvelope(t *testing.T) {
	added := ResponseOutputItem{ID: "msg_1", Type: "message", Status: "in_progress", Role: "assistant", Content: []ResponseText{}}
	idx := 0
	b, err := json.Marshal(ResponseStreamEvent{Type: "response.output_item.added", OutputIndex: &idx, Item: &added})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"content":[]`) {
		t.Fatalf("response.output_item.added must open the message with an empty content array: %s", b)
	}

	resp := Response{ID: "resp_1", Object: ObjectResponse, Status: "completed", Model: "gpt-test", Output: []ResponseOutputItem{
		{ID: "rs_1", Type: "reasoning", Status: "completed", EncryptedContent: "blob"},
	}}
	b, err = json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"summary":[]`) {
		t.Fatalf("reasoning item in response.output must carry an empty summary array: %s", b)
	}
}
