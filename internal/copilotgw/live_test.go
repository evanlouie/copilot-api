package copilotgw

import (
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
)

const liveTestsEnv = "COPILOT_API_LIVE_TESTS"

func newLiveGateway(t *testing.T) *RealGateway {
	t.Helper()
	if os.Getenv(liveTestsEnv) != "1" {
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
		t.Fatalf("prepare live Copilot session store: %v", err)
	}
	gw := NewReal(cfg, store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := gw.Start(t.Context()); err != nil {
		t.Fatalf("start live Copilot gateway (check COPILOT_CLI_PATH and Copilot authentication): %v", err)
	}
	t.Cleanup(func() {
		if err := gw.Stop(); err != nil {
			// The SDK can report a process-reap timeout after the request itself has
			// completed. Keep that teardown signal distinct from request/backend
			// failures; goleak still catches every non-SDK-owned goroutine below.
			t.Logf("live Copilot SDK/runtime teardown reported an error: %v", err)
		}
	})
	return gw
}

func selectLiveModel(models []Model, eligible func(Model) bool) string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if model.ID != "" && eligible(model) {
			ids = append(ids, model.ID)
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) == 0 {
		return ""
	}
	return ids[0]
}

func requireLiveModel(t *testing.T, gw *RealGateway, envName, requirement string, eligible func(Model) bool) string {
	t.Helper()
	models, err := gw.ListModels(t.Context())
	if err != nil {
		t.Fatalf("list live Copilot models (authentication or backend failure): %v", err)
	}
	if requested := os.Getenv(envName); requested != "" {
		found := false
		for _, model := range models {
			if model.ID != requested {
				continue
			}
			found = true
			if eligible(model) {
				t.Logf("using live Copilot model %q from %s", requested, envName)
				return requested
			}
		}
		if found {
			t.Fatalf("%s=%q names an advertised model that does not satisfy %s", envName, requested, requirement)
		}
		t.Fatalf("%s=%q is not advertised by the live Copilot model catalog", envName, requested)
	}
	model := selectLiveModel(models, eligible)
	if model == "" {
		t.Skipf("live Copilot model catalog advertises no model satisfying %s", requirement)
	}
	t.Logf("using live Copilot model %q selected from the current catalog (%s)", model, requirement)
	return model
}

func supportsTextCompletion(model Model) bool {
	// Every entry returned by the gateway's model catalog is a generation model;
	// the only invariant a plain-text smoke test needs is an addressable ID.
	return model.ID != ""
}

func supportsHighReasoning(model Model) bool {
	if model.ID == "" || !model.ReasoningEffortKnown || !model.SupportsReasoningEffort {
		return false
	}
	// Some catalog versions advertise support without enumerating the accepted
	// levels. When levels are present, require the exact level these tests send.
	return len(model.SupportedReasoningEfforts) == 0 || slices.Contains(model.SupportedReasoningEfforts, "high")
}

func liveTextModel(t *testing.T, gw *RealGateway) string {
	t.Helper()
	return requireLiveModel(t, gw, "COPILOT_API_LIVE_MODEL", "plain text completion", supportsTextCompletion)
}

func liveHighReasoningModel(t *testing.T, gw *RealGateway) string {
	t.Helper()
	return requireLiveModel(t, gw, "COPILOT_API_LIVE_REASONING_MODEL", "reasoning effort high", supportsHighReasoning)
}

func TestSelectLiveModelFiltersCapabilitiesAndSorts(t *testing.T) {
	t.Parallel()
	models := []Model{
		{ID: "z-model", ReasoningEffortKnown: true, SupportsReasoningEffort: true, SupportedReasoningEfforts: []string{"high"}},
		{ID: "b-model", ReasoningEffortKnown: true, SupportsReasoningEffort: true, SupportedReasoningEfforts: []string{"low"}},
		{ID: "a-model", ReasoningEffortKnown: true, SupportsReasoningEffort: true, SupportedReasoningEfforts: []string{"medium", "high"}},
		{ID: "", ReasoningEffortKnown: true, SupportsReasoningEffort: true, SupportedReasoningEfforts: []string{"high"}},
	}
	if got := selectLiveModel(models, supportsHighReasoning); got != "a-model" {
		t.Fatalf("selected model = %q, want the first eligible current-catalog id", got)
	}
}

func TestSupportsHighReasoningUsesAdvertisedCapabilities(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		model Model
		want  bool
	}{
		{name: "known high", model: Model{ID: "a", ReasoningEffortKnown: true, SupportsReasoningEffort: true, SupportedReasoningEfforts: []string{"high"}}, want: true},
		{name: "support without enumerated levels", model: Model{ID: "a", ReasoningEffortKnown: true, SupportsReasoningEffort: true}, want: true},
		{name: "known low only", model: Model{ID: "a", ReasoningEffortKnown: true, SupportsReasoningEffort: true, SupportedReasoningEfforts: []string{"low"}}},
		{name: "known unsupported", model: Model{ID: "a", ReasoningEffortKnown: true}},
		{name: "unknown support", model: Model{ID: "a", SupportsReasoningEffort: true}},
		{name: "empty id", model: Model{ReasoningEffortKnown: true, SupportsReasoningEffort: true, SupportedReasoningEfforts: []string{"high"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := supportsHighReasoning(tt.model); got != tt.want {
				t.Fatalf("supportsHighReasoning(%#v) = %t, want %t", tt.model, got, tt.want)
			}
		})
	}
}

func TestLiveCopilotTextCompletion(t *testing.T) {
	t.Parallel()
	gw := newLiveGateway(t)
	model := liveTextModel(t, gw)
	turn, err := gw.Chat(t.Context(), ChatRequest{OpenAIID: openai.NewID("chatcmpl_"), Model: model, FinalUser: openai.ChatMessage{Role: "user", Content: openai.NewTextContent("Reply with OK only.")}})
	if err != nil {
		t.Fatalf("live Copilot backend request with advertised model %q failed: %v", model, err)
	}
	if turn.Text == "" {
		t.Fatal("empty live response")
	}
}

// TestLiveCopilotReasoningStreamsBeforeContent formalizes the throwaway spike
// probe: with a reasoning-capable model at high effort, the gateway must stream
// reasoning deltas before any visible content delta. This is the live
// counterpart to the deterministic encoder ordering tests.
func TestLiveCopilotReasoningStreamsBeforeContent(t *testing.T) {
	t.Parallel()
	gw := newLiveGateway(t)
	model := liveHighReasoningModel(t, gw)

	ch, err := gw.StreamChat(t.Context(), ChatRequest{
		OpenAIID:        openai.NewID("chatcmpl_"),
		Model:           model,
		ReasoningEffort: "high",
		FinalUser:       openai.ChatMessage{Role: "user", Content: openai.NewTextContent("Think step by step, then answer: what is 17 * 23?")},
	})
	if err != nil {
		t.Fatalf("start live Copilot reasoning stream with advertised model %q: %v", model, err)
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
			t.Fatalf("live Copilot backend reasoning stream with model %q failed: %v", model, ev.Error)
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
	model := liveHighReasoningModel(t, gw)

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
		t.Fatalf("start live Copilot tool stream with advertised model %q: %v", model, err)
	}
	var first *TurnResult
	for ev := range ch {
		switch ev.Kind {
		case "result":
			first = ev.Result
		case "error":
			t.Fatalf("first live Copilot tool stream with model %q failed: %v", model, ev.Error)
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
		t.Fatalf("start live Copilot continuation stream with advertised model %q: %v", model, err)
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
			t.Fatalf("live Copilot continuation stream with model %q failed: %v", model, ev.Error)
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
