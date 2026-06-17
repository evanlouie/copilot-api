package copilotgw

import (
	"encoding/json"
	"testing"

	"github.com/evanlouie/copilot-api/internal/openai"
)

func TestWarmResponseSessionUseInheritsWarmRequestState(t *testing.T) {
	warm := &WarmResponseSession{
		responseID:      "resp_warm",
		model:           "gpt-5",
		instructions:    "Be concise.",
		reasoningEffort: "low",
		tools:           []openai.NormalizedTool{{Kind: openai.ToolKindFunction, Name: "lookup"}},
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
	if len(req.Tools) != 1 || req.Tools[0].Name != "lookup" {
		t.Fatalf("request tools = %#v, want warm lookup tool", req.Tools)
	}
	if req.Input.Text != "Warm context\n\nCurrent turn" {
		t.Fatalf("combined input = %q", req.Input.Text)
	}
}

func TestWarmResponseSessionUseAcceptsSemanticEquivalentToolCatalog(t *testing.T) {
	warm := &WarmResponseSession{
		responseID: "resp_warm",
		model:      "gpt-5",
		tools: []openai.NormalizedTool{
			{Kind: openai.ToolKindFunction, Name: "lookup", Parameters: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`), Raw: json.RawMessage(`{"type":"function","name":"lookup","parameters":{"properties":{"q":{"type":"string"}},"type":"object"}}`)},
			{Kind: openai.ToolKindCustom, Name: "apply_patch", Format: json.RawMessage(`{"syntax":"lark","type":"grammar"}`), Raw: json.RawMessage(`{"name":"apply_patch","type":"custom"}`)},
		},
	}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", Tools: []openai.NormalizedTool{
		{Kind: openai.ToolKindCustom, Name: "apply_patch", Format: json.RawMessage(`{"type":"grammar","syntax":"lark"}`), Raw: json.RawMessage(`{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark"}}`)},
		{Kind: openai.ToolKindFunction, Name: "lookup", Parameters: json.RawMessage(` { "properties" : { "q" : { "type" : "string" } }, "type" : "object" } `), Raw: json.RawMessage(`{"different":"raw should not affect reuse"}`)},
	}}
	if _, _, _, _, _, ok := warm.use(&req); !ok {
		t.Fatal("warm session was not used for semantically equivalent tool catalog")
	}
}

func TestWarmResponseSessionUseRejectsSemanticToolCatalogMismatch(t *testing.T) {
	warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", tools: []openai.NormalizedTool{{Kind: openai.ToolKindCustom, Name: "apply_patch", Format: json.RawMessage(`{"type":"grammar","syntax":"lark"}`)}}}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", Tools: []openai.NormalizedTool{{Kind: openai.ToolKindCustom, Name: "apply_patch", Format: json.RawMessage(`{"type":"grammar","syntax":"regex"}`)}}}
	if _, _, _, _, _, ok := warm.use(&req); ok {
		t.Fatal("warm session used despite semantic tool catalog mismatch")
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
