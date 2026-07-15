package copilotgw

import (
	"encoding/json"
	"testing"

	"github.com/evanlouie/copilot-api/internal/openai"
	copilot "github.com/github/copilot-sdk/go"
)

func TestWarmResponseSessionUseInheritsWarmRequestState(t *testing.T) {
	warm := &WarmResponseSession{
		responseID:      "resp_warm",
		model:           "gpt-5",
		instructions:    "Be concise.",
		reasoningEffort: "low",
		tools:           []openai.NormalizedTool{{Kind: openai.ToolKindFunction, Name: "lookup"}},
		input:           resolvedPrompt{Text: "Warm context"},
	}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", Input: openai.PromptContent{Text: "Current turn"}}

	used, ok := warm.use(&req)
	if !ok {
		t.Fatal("warm session was not used")
	}
	if used.previous == nil || *used.previous != "resp_warm" {
		t.Fatalf("previous = %#v, want resp_warm", used.previous)
	}
	if req.Instructions != "Be concise." || req.ReasoningEffort != "low" {
		t.Fatalf("request did not inherit instructions/reasoning: %#v", req)
	}
	if len(req.Tools) != 1 || req.Tools[0].Name != "lookup" {
		t.Fatalf("request tools = %#v, want warm lookup tool", req.Tools)
	}
	combined := combineResolvedPrompts(used.prompt, resolvedPrompt{Text: req.Input.Text})
	if combined.Text != "Warm context\n\nCurrent turn" {
		t.Fatalf("combined input = %q", combined.Text)
	}
}

func TestWarmResponseSessionTransfersResolvedImagesAndModelCount(t *testing.T) {
	data := "aW1hZ2U="
	attachment := copilot.AttachmentBlob{Data: &data, MIMEType: "image/png"}
	budget := &imageRequestBudget{configured: true, maxImages: 2, remainingImages: 1}
	pinReleased := false
	warm := &WarmResponseSession{
		responseID: "resp_warm", model: "gpt-5",
		input:       resolvedPrompt{Text: "warm", Attachments: []copilot.Attachment{attachment}},
		imageBudget: budget, pinReleases: []func(){func() { pinReleased = true }},
	}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm"}
	used, ok := warm.use(&req)
	if !ok || len(used.prompt.Attachments) != 1 || used.imageBudget != budget || len(used.pinReleases) != 1 {
		t.Fatalf("warm transfer = %#v, ok=%v", used, ok)
	}
	combined := combineResolvedPrompts(used.prompt, resolvedPrompt{Attachments: []copilot.Attachment{attachment}})
	if len(combined.Attachments) != 2 {
		t.Fatalf("combined attachments = %d", len(combined.Attachments))
	}
	releaseAll(used.pinReleases)
	if !pinReleased {
		t.Fatal("pin ownership was not released")
	}
}

func TestRejectedWarmResponseDisconnectReleasesPins(t *testing.T) {
	pinReleased := false
	warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", pinReleases: []func(){func() { pinReleased = true }}}
	req := ResponseRequest{Model: "other", PreviousResponseID: "resp_warm"}
	if _, ok := warm.use(&req); ok {
		t.Fatal("mismatched warm response was used")
	}
	warm.Disconnect()
	if !pinReleased {
		t.Fatal("warm response pin was not released")
	}
}

func TestWarmResponseSessionUseInheritsResolvedDynamicCatalog(t *testing.T) {
	warm := &WarmResponseSession{
		responseID: "resp_warm",
		model:      "gpt-5",
		tools: []openai.NormalizedTool{
			{Kind: openai.ToolKindToolSearch, Name: "tool_search", Execution: "client"},
			{Kind: openai.ToolKindNamespace, Name: "multi_agent_v1", Children: []openai.NormalizedTool{{Kind: openai.ToolKindFunction, Name: "spawn_agent"}}},
		},
	}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm"}
	_, ok := warm.use(&req)
	if !ok {
		t.Fatal("warm session was not used")
	}
	if len(req.Tools) != 2 || req.Tools[1].Kind != openai.ToolKindNamespace || req.Tools[1].Children[0].Name != "spawn_agent" {
		t.Fatalf("request tools = %#v, want resolved dynamic catalog", req.Tools)
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
	if _, ok := warm.use(&req); !ok {
		t.Fatal("warm session was not used for semantically equivalent tool catalog")
	}
}

func TestWarmResponseSessionUseRejectsSemanticToolCatalogMismatch(t *testing.T) {
	warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", tools: []openai.NormalizedTool{{Kind: openai.ToolKindCustom, Name: "apply_patch", Format: json.RawMessage(`{"type":"grammar","syntax":"lark"}`)}}}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", Tools: []openai.NormalizedTool{{Kind: openai.ToolKindCustom, Name: "apply_patch", Format: json.RawMessage(`{"type":"grammar","syntax":"regex"}`)}}}
	if _, ok := warm.use(&req); ok {
		t.Fatal("warm session used despite semantic tool catalog mismatch")
	}
}

func TestWarmResponseSessionUseAcceptsEquivalentExplicitReasoningEffort(t *testing.T) {
	warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", reasoningEffort: "low"}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", ReasoningEffort: " LOW "}
	if _, ok := warm.use(&req); !ok {
		t.Fatal("warm session was not used for equivalent explicit reasoning effort")
	}
}

func TestWarmResponseSessionUseRejectsMismatchedReasoningEffort(t *testing.T) {
	warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", reasoningEffort: "low"}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", ReasoningEffort: "high"}
	if _, ok := warm.use(&req); ok {
		t.Fatal("warm session used despite mismatched reasoning effort")
	}
}

func TestWarmResponseSessionUseRejectsMismatchedInstructions(t *testing.T) {
	warm := &WarmResponseSession{responseID: "resp_warm", model: "gpt-5", instructions: "original"}
	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm", Instructions: "changed"}
	if _, ok := warm.use(&req); ok {
		t.Fatal("warm session used despite mismatched instructions")
	}
}
