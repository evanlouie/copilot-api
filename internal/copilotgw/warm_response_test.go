package copilotgw

import (
	"testing"

	"github.com/evanlouie/copilot-api/internal/openai"
)

func TestWarmResponseSessionUseInheritsWarmRequestState(t *testing.T) {
	warm := &WarmResponseSession{
		responseID:      "resp_warm",
		model:           "gpt-5",
		instructions:    "Be concise.",
		reasoningEffort: "low",
		tools:           []openai.Tool{{Type: "function", Function: openai.FunctionTool{Name: "lookup"}}},
		input:           openai.PromptContent{Text: "Warm context"},
	}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", Input: openai.PromptContent{Text: "Current turn"}}

	_, _, _, _, previous, ok := warm.use(&req)
	if !ok {
		t.Fatal("warm session was not used")
	}
	if previous == nil || *previous != "resp_warm" {
		t.Fatalf("previous = %#v, want resp_warm", previous)
	}
	if req.Instructions != "Be concise." || req.ReasoningEffort != "low" {
		t.Fatalf("request did not inherit instructions/reasoning: %#v", req)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "lookup" {
		t.Fatalf("request tools = %#v, want warm lookup tool", req.Tools)
	}
	if req.Input.Text != "Warm context\n\nCurrent turn" {
		t.Fatalf("combined input = %q", req.Input.Text)
	}
}

func TestWarmResponseSessionUseAcceptsEquivalentExplicitReasoningEffort(t *testing.T) {
	warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", reasoningEffort: "low"}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", ReasoningEffort: " LOW "}
	if _, _, _, _, _, ok := warm.use(&req); !ok {
		t.Fatal("warm session was not used for equivalent explicit reasoning effort")
	}
}

func TestWarmResponseSessionUseRejectsMismatchedReasoningEffort(t *testing.T) {
	warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", reasoningEffort: "low"}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", ReasoningEffort: "high"}
	if _, _, _, _, _, ok := warm.use(&req); ok {
		t.Fatal("warm session used despite mismatched reasoning effort")
	}
}

func TestWarmResponseSessionUseRejectsMismatchedInstructions(t *testing.T) {
	warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", instructions: "original"}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", Instructions: "changed"}
	if _, _, _, _, _, ok := warm.use(&req); ok {
		t.Fatal("warm session used despite mismatched instructions")
	}
}
