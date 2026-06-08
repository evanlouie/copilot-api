package copilotgw

import (
	"testing"

	"github.com/evanlouie/copilot-api/internal/openai"
)

func TestResolveReasoningPrefersConsolidated(t *testing.T) {
	if got := resolveReasoning("final", "deltas"); got != "final" {
		t.Fatalf("resolveReasoning(final, deltas) = %q, want final", got)
	}
	if got := resolveReasoning("", "deltas"); got != "deltas" {
		t.Fatalf("resolveReasoning(empty, deltas) = %q, want deltas fallback", got)
	}
	if got := resolveReasoning("", ""); got != "" {
		t.Fatalf("resolveReasoning(empty, empty) = %q, want empty", got)
	}
}

func TestResponseFromTurnPrependsReasoningItem(t *testing.T) {
	turn := &TurnResult{
		Text:               "the answer",
		Reasoning:          "let me think",
		ReasoningEncrypted: "enc-blob",
		ReasoningID:        "rid-7",
		FinishReason:       "stop",
	}
	resp := responseFromTurn("resp_1", "claude-sonnet-4.6", "", nil, true, turn)
	if len(resp.Output) != 2 {
		t.Fatalf("output length = %d, want reasoning + message: %#v", len(resp.Output), resp.Output)
	}
	reasoning := resp.Output[0]
	if reasoning.Type != "reasoning" || reasoning.ID != "rs_rid-7" || reasoning.EncryptedContent != "enc-blob" {
		t.Fatalf("reasoning item = %#v", reasoning)
	}
	if len(reasoning.Summary) != 1 || reasoning.Summary[0].Type != "summary_text" || reasoning.Summary[0].Text != "let me think" {
		t.Fatalf("reasoning summary = %#v", reasoning.Summary)
	}
	if resp.Output[1].Type != "message" {
		t.Fatalf("second item = %#v, want message after reasoning", resp.Output[1])
	}
}

func TestResponseFromTurnWithoutReasoningHasNoReasoningItem(t *testing.T) {
	turn := &TurnResult{Text: "hi", FinishReason: "stop"}
	resp := responseFromTurn("resp_1", "gpt-5", "", nil, true, turn)
	for _, item := range resp.Output {
		if item.Type == "reasoning" {
			t.Fatalf("unexpected reasoning item: %#v", resp.Output)
		}
	}
}

func TestResponseFromTurnReasoningPrecedesToolCalls(t *testing.T) {
	turn := &TurnResult{
		Reasoning:    "consider the tool",
		ReasoningID:  "rid-9",
		FinishReason: "tool_calls",
		ToolCalls: []openai.ChatToolCall{{
			ID:       "call_1",
			Type:     "function",
			Function: openai.ToolCallFunction{Name: "lookup", Arguments: `{"q":"x"}`},
		}},
	}
	resp := responseFromTurn("resp_1", "claude-sonnet-4.6", "", nil, true, turn)
	if len(resp.Output) != 2 || resp.Output[0].Type != "reasoning" || resp.Output[1].Type != "function_call" {
		t.Fatalf("output = %#v, want reasoning then function_call", resp.Output)
	}
}
