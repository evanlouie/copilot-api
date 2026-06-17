package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

func TestParseResponsesInputAcceptsCustomAndToolSearchOutputs(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"custom_tool_call_output","call_id":"call_patch","name":"apply_patch","output":[{"type":"output_text","text":"patched"}]},
		{"type":"tool_search_output","call_id":"call_search","execution":"client","status":"completed","tools":[{"type":"function","name":"loaded_tool","parameters":{"type":"object","properties":{}}}]}
	]`)
	prompt, outputs, instructions, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if prompt.Text != "" || instructions != "" {
		t.Fatalf("prompt/instructions = %#v/%q, want empty", prompt, instructions)
	}
	if got := outputs["call_patch"]; got.Kind != openai.ToolKindCustom || got.Name != "apply_patch" || got.Output != "patched" {
		t.Fatalf("custom output = %#v", got)
	}
	if got := outputs["call_search"]; got.Kind != openai.ToolKindToolSearch || got.Execution != "client" || got.Status != "completed" || !strings.Contains(got.Output, "loaded_tool") || len(got.LoadedTools) != 1 || got.LoadedTools[0].Name != "loaded_tool" {
		t.Fatalf("tool_search output = %#v", got)
	}
}

func TestParseResponsesInputRejectsUnsafeToolSearchOutputTools(t *testing.T) {
	raw := json.RawMessage(`[{"type":"tool_search_output","call_id":"call_search","execution":"client","tools":[{"type":"custom","name":"apply_patch"}]}]`)
	_, _, _, err := parseResponsesInput(raw)
	if err == nil || !strings.Contains(err.Error(), "tool_search_output.tools may only contain") {
		t.Fatalf("error = %v, want loadable-tool rejection", err)
	}
}

func TestParseResponsesInputRejectsToolSearchOutputAliasCollisions(t *testing.T) {
	raw := json.RawMessage(`[{"type":"tool_search_output","call_id":"call_search","execution":"client","tools":[{"type":"function","name":"lookup__child"},{"type":"namespace","name":"lookup","tools":[{"name":"child"}]}]}]`)
	_, _, _, err := parseResponsesInput(raw)
	if err == nil || !strings.Contains(err.Error(), "SDK name collision") {
		t.Fatalf("error = %v, want alias collision", err)
	}
}

func TestResponseStreamEmitsExtendedToolCallItems(t *testing.T) {
	resp := &openai.Response{ID: "resp_1", Object: openai.ObjectResponse, CreatedAt: openai.UnixNow(), Status: "completed", Model: "gpt-test", Output: []openai.ResponseOutputItem{
		{ID: "ctc_call_patch", Type: "custom_tool_call", Status: "completed", CallID: "call_patch", Name: "apply_patch", Input: "*** Begin Patch"},
		{ID: "tsc_call_search", Type: "tool_search_call", Status: "completed", CallID: "call_search", Execution: "client", ArgumentsJSON: json.RawMessage(`{"query":"grep"}`)},
	}, ParallelToolCalls: true, Store: true}
	writer := &captureResponseEventWriter{}
	result := writeResponseStreamEvents(nilContext(), writer, copilotgw.ResponseRequest{ResponseID: "resp_1", Model: "gpt-test", Store: true}, streamResponse(resp))
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	wantTypes := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.output_item.done",
		"response.output_item.added",
		"response.output_item.done",
		"response.completed",
	}
	if got := writer.types(); strings.Join(got, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("event order = %v, want %v", got, wantTypes)
	}
	for i, ev := range writer.events {
		if ev.SequenceNumber != int64(i) {
			t.Fatalf("sequence_number[%d] = %d, want %d", i, ev.SequenceNumber, i)
		}
	}
	checks := []struct {
		eventIndex int
		itemType   string
		status     string
		output     int
	}{
		{eventIndex: 2, itemType: "custom_tool_call", status: "in_progress", output: 0},
		{eventIndex: 3, itemType: "custom_tool_call", status: "completed", output: 0},
		{eventIndex: 4, itemType: "tool_search_call", status: "in_progress", output: 1},
		{eventIndex: 5, itemType: "tool_search_call", status: "completed", output: 1},
	}
	for _, check := range checks {
		ev := writer.events[check.eventIndex]
		if ev.Item == nil || ev.Item.Type != check.itemType || ev.Item.Status != check.status {
			t.Fatalf("event[%d] item = %#v, want %s/%s", check.eventIndex, ev.Item, check.itemType, check.status)
		}
		if ev.OutputIndex == nil || *ev.OutputIndex != check.output {
			t.Fatalf("event[%d] output_index = %#v, want %d", check.eventIndex, ev.OutputIndex, check.output)
		}
	}
}

type captureResponseEventWriter struct{ events []openai.ResponseStreamEvent }

func (w *captureResponseEventWriter) WriteResponseEvent(ev openai.ResponseStreamEvent) error {
	w.events = append(w.events, ev)
	return nil
}

func (w *captureResponseEventWriter) types() []string {
	out := make([]string, 0, len(w.events))
	for _, ev := range w.events {
		out = append(out, ev.Type)
	}
	return out
}

func (w *captureResponseEventWriter) itemsOfType(typ string) []openai.ResponseOutputItem {
	var out []openai.ResponseOutputItem
	for _, ev := range w.events {
		if ev.Item != nil && ev.Item.Type == typ {
			out = append(out, *ev.Item)
		}
	}
	return out
}

func streamResponse(resp *openai.Response) <-chan copilotgw.ResponseStreamEvent {
	ch := make(chan copilotgw.ResponseStreamEvent, 1)
	ch <- copilotgw.ResponseStreamEvent{Kind: "response", Response: resp}
	close(ch)
	return ch
}

func nilContext() context.Context { return context.Background() }

func containsEventType(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
