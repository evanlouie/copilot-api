package copilotgw

import (
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
)

func TestLiveCopilotTextCompletion(t *testing.T) {
	t.Parallel()
	gw := newLiveGateway(t)
	model := liveModel(t, gw, "COPILOT_API_LIVE_MODEL", "basic chat completion", func(model Model) bool {
		// The Copilot model catalog contains inference models only. A non-empty ID
		// is therefore the complete capability requirement for this smoke test.
		return strings.TrimSpace(model.ID) != ""
	})
	turn, err := gw.Chat(t.Context(), ChatRequest{OpenAIID: openai.NewID("chatcmpl_"), Model: model, FinalUser: openai.ChatMessage{Role: "user", Content: openai.NewTextContent("Reply with OK only.")}})
	if err != nil {
		t.Fatalf("live text completion with model %q failed: %v", model, err)
	}
	if turn.Text == "" {
		t.Fatalf("live text completion with model %q returned an empty response", model)
	}
}

// TestLiveCopilotReasoningStreamsBeforeContent formalizes the throwaway spike
// probe: with a reasoning-capable model at high effort, the gateway must stream
// reasoning deltas before any visible content delta. This is the live
// counterpart to the deterministic encoder ordering tests.
func TestLiveCopilotReasoningStreamsBeforeContent(t *testing.T) {
	t.Parallel()
	gw := newLiveGateway(t)
	model := liveReasoningModel(t, gw)

	ch, err := gw.StreamChat(t.Context(), ChatRequest{
		OpenAIID:        openai.NewID("chatcmpl_"),
		Model:           model,
		ReasoningEffort: "high",
		FinalUser:       openai.ChatMessage{Role: "user", Content: openai.NewTextContent("Think step by step, then answer: what is 17 * 23?")},
	})
	if err != nil {
		t.Fatalf("start live reasoning stream with model %q: %v", model, err)
	}
	sawReasoning := false
	sawContentBeforeReasoning := false
	sawContent := false
	gotResult := false
	var finalText string
	for ev := range ch {
		switch ev.Kind {
		case "reasoning_delta":
			if ev.Delta != "" {
				sawReasoning = true
			}
		case "delta":
			if ev.Delta != "" {
				sawContent = true
				if !sawReasoning {
					sawContentBeforeReasoning = true
				}
			}
		case "result":
			gotResult = true
			if ev.Result != nil {
				finalText = ev.Result.Text
				if ev.Result.Reasoning == "" {
					t.Error("final turn result carried no reasoning text")
				}
			}
		case "error":
			t.Fatalf("live reasoning stream with model %q failed: %v", model, ev.Error)
		}
	}
	if !gotResult {
		t.Fatal("stream ended without a terminal result")
	}
	if !sawReasoning {
		t.Fatal("expected at least one reasoning delta before content")
	}
	if !sawContent {
		t.Fatal("expected at least one content delta after reasoning")
	}
	if sawContentBeforeReasoning {
		t.Fatal("content delta arrived before any reasoning delta")
	}
	if finalText == "" {
		t.Fatal("final turn result carried no answer text")
	}
}

func TestLiveCopilotReasoningAfterToolContinuation(t *testing.T) {
	t.Parallel()
	gw := newLiveGateway(t)
	model := liveReasoningModel(t, gw)

	tools := []openai.Tool{{
		Type: "function",
		Function: openai.FunctionTool{
			Name:        "get_weather",
			Description: "Return the current weather for a city.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
		},
	}}
	ch, err := gw.StreamChat(t.Context(), ChatRequest{
		OpenAIID:        openai.NewID("chatcmpl_"),
		Model:           model,
		ReasoningEffort: "high",
		FinalUser:       openai.ChatMessage{Role: "user", Content: openai.NewTextContent("Use get_weather exactly once for Tokyo, then answer with the weather summary.")},
		Tools:           tools,
	})
	if err != nil {
		t.Fatalf("start live tool-call stream with model %q: %v", model, err)
	}
	var first *TurnResult
	for ev := range ch {
		switch ev.Kind {
		case "result":
			first = ev.Result
		case "error":
			t.Fatalf("first live tool-call stream with model %q failed: %v", model, ev.Error)
		}
	}
	if first == nil || len(first.ToolCalls) == 0 {
		t.Fatalf("expected first turn to request get_weather, got %#v", first)
	}
	if first.Reasoning == "" {
		t.Fatal("first tool-call turn carried no reasoning")
	}

	outputs := map[string]string{}
	for _, call := range first.ToolCalls {
		outputs[call.ID] = `{"city":"Tokyo","condition":"sunny","temperature_c":22}`
	}
	ch2, err := gw.StreamContinueChatToolCalls(t.Context(), ChatContinuationRequest{
		Model:           model,
		Outputs:         outputs,
		Tools:           tools,
		ReasoningEffort: "high",
	})
	if err != nil {
		t.Fatalf("continue live tool-call stream with model %q: %v", model, err)
	}
	var second *TurnResult
	sawSecondReasoningDelta := false
	for ev := range ch2 {
		switch ev.Kind {
		case "reasoning_delta":
			if ev.Delta != "" {
				sawSecondReasoningDelta = true
			}
		case "result":
			second = ev.Result
		case "error":
			t.Fatalf("continuation live stream with model %q failed: %v", model, ev.Error)
		}
	}
	if second == nil {
		t.Fatal("continuation stream ended without result")
	}
	if !sawSecondReasoningDelta || second.Reasoning == "" {
		t.Fatalf("continuation did not produce fresh streamed reasoning: sawDelta=%v result=%#v", sawSecondReasoningDelta, second)
	}
	if first.ReasoningID != "" && second.ReasoningID != "" && first.ReasoningID == second.ReasoningID {
		t.Fatalf("continuation reused reasoning id %q", first.ReasoningID)
	}
	if second.ReasoningOpaque == "" && second.ReasoningEncrypted == "" {
		t.Fatalf("continuation reasoning lacked opaque/encrypted continuity fields: %#v", second)
	}
}

func newLiveGateway(t *testing.T) *RealGateway {
	t.Helper()
	if os.Getenv("COPILOT_API_LIVE_TESTS") != "1" {
		t.Skip("set COPILOT_API_LIVE_TESTS=1 to run live Copilot integration tests")
	}
	root := t.TempDir()
	cfg := config.Config{
		DataDir:        root + "/data",
		StateDir:       root + "/state",
		ConfigDir:      root + "/config",
		ToolCallTTL:    time.Minute,
		ModelsCacheTTL: time.Minute,
		GitHubToken:    os.Getenv("GITHUB_TOKEN"),
	}
	store := sessionstore.New(cfg.DataDir, cfg.StateDir)
	if err := store.Ensure(); err != nil {
		t.Fatalf("prepare live test state: %v", err)
	}
	gw := NewReal(cfg, store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := gw.Start(t.Context()); err != nil {
		t.Fatalf("start live Copilot gateway (authenticate with GITHUB_TOKEN or a logged-in Copilot CLI): %v", err)
	}
	t.Cleanup(func() {
		if err := gw.Stop(); err != nil {
			t.Errorf("stop live Copilot gateway: %v", err)
		}
	})
	return gw
}

func liveReasoningModel(t *testing.T, gw *RealGateway) string {
	t.Helper()
	return liveModel(t, gw, "COPILOT_API_LIVE_REASONING_MODEL", "high reasoning effort", func(model Model) bool {
		if strings.TrimSpace(model.ID) == "" || !model.SupportsReasoningEffort {
			return false
		}
		return len(model.SupportedReasoningEfforts) == 0 || slices.Contains(model.SupportedReasoningEfforts, "high")
	})
}

func liveModel(t *testing.T, gw *RealGateway, envName, capability string, eligible func(Model) bool) string {
	t.Helper()
	models, err := gw.ListModels(t.Context())
	if err != nil {
		t.Fatalf("list live Copilot models (catalog/backend failure): %v", err)
	}
	requested := strings.TrimSpace(os.Getenv(envName))
	model, reason := chooseLiveModel(models, requested, eligible)
	if model == "" {
		t.Skipf("no live model available for %s: %s", capability, reason)
	}
	return model
}

func chooseLiveModel(models []Model, requested string, eligible func(Model) bool) (string, string) {
	if requested != "" {
		for _, model := range models {
			if model.ID != requested {
				continue
			}
			if eligible(model) {
				return model.ID, ""
			}
			return "", "requested model " + requested + " is advertised but lacks the required capability"
		}
		return "", "requested model " + requested + " is not advertised; available models: " + liveModelIDs(models)
	}
	for _, model := range models {
		if eligible(model) {
			return model.ID, ""
		}
	}
	return "", "catalog has no eligible model; available models: " + liveModelIDs(models)
}

func liveModelIDs(models []Model) string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if id := strings.TrimSpace(model.ID); id != "" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return "<none>"
	}
	return strings.Join(ids, ", ")
}

func TestChooseLiveModel(t *testing.T) {
	t.Parallel()
	models := []Model{
		{ID: "plain"},
		{ID: "reasoning", SupportsReasoningEffort: true, SupportedReasoningEfforts: []string{"low", "high"}},
	}
	reasoning := func(model Model) bool {
		return model.SupportsReasoningEffort && slices.Contains(model.SupportedReasoningEfforts, "high")
	}
	if got, reason := chooseLiveModel(models, "reasoning", reasoning); got != "reasoning" || reason != "" {
		t.Fatalf("requested eligible model = %q, %q; want reasoning, empty reason", got, reason)
	}
	if got, reason := chooseLiveModel(models, "plain", reasoning); got != "" || !strings.Contains(reason, "lacks the required capability") {
		t.Fatalf("requested ineligible model = %q, %q; want capability rejection", got, reason)
	}
	if got, reason := chooseLiveModel(models, "missing", reasoning); got != "" || !strings.Contains(reason, "not advertised") {
		t.Fatalf("missing requested model = %q, %q; want catalog rejection", got, reason)
	}
	if got, reason := chooseLiveModel(models, "", reasoning); got != "reasoning" || reason != "" {
		t.Fatalf("automatic model = %q, %q; want reasoning, empty reason", got, reason)
	}
}
