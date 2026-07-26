package httpapi

import (
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

// A function call whose arguments arrive as fragments is announced once, then
// extended, and closed with exactly the terminal events an unstreamed call
// produces - so a client can reconcile the two paths without knowing which one
// it got.
func TestResponseStreamForwardsFunctionCallArgumentFragments(t *testing.T) {
	t.Parallel()
	inProgress := openai.ResponseOutputItem{ID: "fc_call_1", Type: "function_call", Status: "in_progress", CallID: "call_1", Name: "lookup"}
	completed := openai.ResponseOutputItem{ID: "fc_call_1", Type: "function_call", Status: "completed", CallID: "call_1", Name: "lookup", Arguments: `{"q":"alpha"}`}
	resp := &openai.Response{ID: "resp_1", Object: openai.ObjectResponse, CreatedAt: openai.UnixNow(), Status: "completed", Model: "gpt-test", Output: []openai.ResponseOutputItem{completed}, ParallelToolCalls: true}
	writer := &captureResponseEventWriter{}

	result := writeResponseStreamEvents(nilContext(), writer, copilotgw.ResponseRequest{ResponseID: "resp_1", Model: "gpt-test"}, 0, false, streamEvents(
		toolCallDeltaEvent(inProgress, `{"q":`),
		toolCallDeltaEvent(inProgress, `"alpha"}`),
		copilotgw.ResponseStreamEvent{Kind: "response", Response: resp},
	))
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	wantTypes := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	}
	if got := writer.types(); strings.Join(got, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("event order = %v, want %v", got, wantTypes)
	}
	if added := writer.events[2]; added.Item == nil || added.Item.Status != "in_progress" || added.Item.Arguments != "" {
		t.Fatalf("announced item = %#v, want an in-progress item with no arguments yet", added.Item)
	}
	if got := streamedDelta(writer.events, "response.function_call_arguments.delta"); got != completed.Arguments {
		t.Fatalf("accumulated fragments = %q, want %q", got, completed.Arguments)
	}
	if done := writer.events[5]; done.Arguments != completed.Arguments || done.Name != "lookup" {
		t.Fatalf("done event = %#v, want the complete arguments and name", done)
	}
	for i, ev := range writer.events {
		if ev.SequenceNumber != int64(i) {
			t.Fatalf("sequence_number[%d] = %d, want %d", i, ev.SequenceNumber, i)
		}
	}
}

// A freeform custom tool streams raw grammar input, which is a different thing
// from JSON arguments and carries its own event name.
func TestResponseStreamForwardsCustomToolCallInputFragments(t *testing.T) {
	t.Parallel()
	inProgress := openai.ResponseOutputItem{ID: "ctc_call_2", Type: "custom_tool_call", Status: "in_progress", CallID: "call_2", Name: "apply_patch"}
	completed := openai.ResponseOutputItem{ID: "ctc_call_2", Type: "custom_tool_call", Status: "completed", CallID: "call_2", Name: "apply_patch", Input: "*** Begin Patch\n*** End Patch"}
	resp := &openai.Response{ID: "resp_1", Object: openai.ObjectResponse, CreatedAt: openai.UnixNow(), Status: "completed", Model: "gpt-test", Output: []openai.ResponseOutputItem{completed}, ParallelToolCalls: true}
	writer := &captureResponseEventWriter{}

	result := writeResponseStreamEvents(nilContext(), writer, copilotgw.ResponseRequest{ResponseID: "resp_1", Model: "gpt-test"}, 0, false, streamEvents(
		toolCallDeltaEvent(inProgress, "*** Begin Patch\n"),
		toolCallDeltaEvent(inProgress, "*** End Patch"),
		copilotgw.ResponseStreamEvent{Kind: "response", Response: resp},
	))
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	wantTypes := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.delta",
		"response.custom_tool_call_input.done",
		"response.output_item.done",
		"response.completed",
	}
	if got := writer.types(); strings.Join(got, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("event order = %v, want %v", got, wantTypes)
	}
	if got := streamedDelta(writer.events, "response.custom_tool_call_input.delta"); got != completed.Input {
		t.Fatalf("accumulated fragments = %q, want %q", got, completed.Input)
	}
	if done := writer.events[5]; done.Input != completed.Input || done.ItemID != completed.ID {
		t.Fatalf("done event = %#v, want the complete input", done)
	}
}

// The turn can still resolve input the fragments never carried, and that suffix
// has to reach the client as a delta before the item is closed.
func TestResponseStreamEmitsTheSuffixFragmentsDidNotCarry(t *testing.T) {
	t.Parallel()
	inProgress := openai.ResponseOutputItem{ID: "fc_call_1", Type: "function_call", Status: "in_progress", CallID: "call_1", Name: "lookup"}
	completed := openai.ResponseOutputItem{ID: "fc_call_1", Type: "function_call", Status: "completed", CallID: "call_1", Name: "lookup", Arguments: `{"q":"alpha"}`}
	resp := &openai.Response{ID: "resp_1", Object: openai.ObjectResponse, CreatedAt: openai.UnixNow(), Status: "completed", Model: "gpt-test", Output: []openai.ResponseOutputItem{completed}, ParallelToolCalls: true}
	writer := &captureResponseEventWriter{}

	result := writeResponseStreamEvents(nilContext(), writer, copilotgw.ResponseRequest{ResponseID: "resp_1", Model: "gpt-test"}, 0, false, streamEvents(
		toolCallDeltaEvent(inProgress, `{"q":`),
		copilotgw.ResponseStreamEvent{Kind: "response", Response: resp},
	))
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if got := streamedDelta(writer.events, "response.function_call_arguments.delta"); got != completed.Arguments {
		t.Fatalf("accumulated fragments = %q, want %q", got, completed.Arguments)
	}
}

// Fragments the finished call contradicts are unrecoverable for a client that
// already accumulated them, so the response fails instead of completing with
// arguments that disagree with the stored record.
func TestResponseStreamFailsWhenToolCallFragmentsDoNotReconcile(t *testing.T) {
	t.Parallel()
	inProgress := openai.ResponseOutputItem{ID: "fc_call_1", Type: "function_call", Status: "in_progress", CallID: "call_1", Name: "lookup"}
	completed := openai.ResponseOutputItem{ID: "fc_call_1", Type: "function_call", Status: "completed", CallID: "call_1", Name: "lookup", Arguments: `{"q":"alpha"}`}
	resp := &openai.Response{ID: "resp_1", Object: openai.ObjectResponse, CreatedAt: openai.UnixNow(), Status: "completed", Model: "gpt-test", Output: []openai.ResponseOutputItem{completed}, ParallelToolCalls: true}
	writer := &captureResponseEventWriter{}

	result := writeResponseStreamEvents(nilContext(), writer, copilotgw.ResponseRequest{ResponseID: "resp_1", Model: "gpt-test"}, 0, false, streamEvents(
		toolCallDeltaEvent(inProgress, `{"q":"beta"}`),
		copilotgw.ResponseStreamEvent{Kind: "response", Response: resp},
	))
	if result.Err == nil || !strings.Contains(result.Err.Error(), "do not match the streamed arguments") {
		t.Fatalf("result = %#v, want a reconciliation failure", result)
	}
	types := writer.types()
	if types[len(types)-1] != "response.failed" {
		t.Fatalf("event order = %v, want a terminal response.failed", types)
	}
	// The item was announced, so it has to be closed even on the failure path.
	var closed bool
	for _, ev := range writer.events {
		if ev.Type == "response.output_item.done" && ev.Item != nil && ev.Item.ID == "fc_call_1" && ev.Item.Status == "incomplete" {
			closed = true
		}
	}
	if !closed {
		t.Fatalf("event order = %v, want the announced tool item closed as incomplete", types)
	}
}

func toolCallDeltaEvent(item openai.ResponseOutputItem, delta string) copilotgw.ResponseStreamEvent {
	return copilotgw.ResponseStreamEvent{Kind: "tool_call_delta", ItemID: item.ID, Item: &item, Delta: delta}
}

func streamEvents(events ...copilotgw.ResponseStreamEvent) <-chan copilotgw.ResponseStreamEvent {
	ch := make(chan copilotgw.ResponseStreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)
	return ch
}

func streamedDelta(events []openai.ResponseStreamEvent, eventType string) string {
	var out strings.Builder
	for _, ev := range events {
		if ev.Type == eventType {
			out.WriteString(ev.Delta)
		}
	}
	return out.String()
}
