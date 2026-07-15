package copilotgw

import (
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
)

func TestLiveCopilotTextCompletion(t *testing.T) {
	if os.Getenv("COPILOT_API_LIVE_TESTS") != "1" {
		t.Skip("set COPILOT_API_LIVE_TESTS=1 to run live Copilot integration tests")
	}
	root := t.TempDir()
	cfg := config.Config{
		DataDir:        root + "/data",
		StateDir:       root + "/state",
		CacheDir:       root + "/cache",
		ConfigDir:      root + "/config",
		ToolCallTTL:    time.Minute,
		ModelsCacheTTL: time.Minute,
		StrictCompat:   true,
		GitHubToken:    os.Getenv("GITHUB_TOKEN"),
	}
	store := sessionstore.New(cfg.DataDir, cfg.StateDir, cfg.CacheDir)
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	gw := NewReal(cfg, store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := gw.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gw.Stop() }()
	turn, err := gw.Chat(t.Context(), ChatRequest{OpenAIID: openai.NewID("chatcmpl_"), Model: "gpt-5", FinalUser: openai.ChatMessage{Role: "user", Content: openai.NewTextContent("Reply with OK only.")}})
	if err != nil {
		t.Fatal(err)
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
	if os.Getenv("COPILOT_API_LIVE_TESTS") != "1" {
		t.Skip("set COPILOT_API_LIVE_TESTS=1 to run live Copilot integration tests")
	}
	model := os.Getenv("COPILOT_API_LIVE_REASONING_MODEL")
	if model == "" {
		model = "claude-sonnet-4.6"
	}
	root := t.TempDir()
	cfg := config.Config{
		DataDir:        root + "/data",
		StateDir:       root + "/state",
		CacheDir:       root + "/cache",
		ConfigDir:      root + "/config",
		ToolCallTTL:    time.Minute,
		ModelsCacheTTL: time.Minute,
		GitHubToken:    os.Getenv("GITHUB_TOKEN"),
	}
	store := sessionstore.New(cfg.DataDir, cfg.StateDir, cfg.CacheDir)
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	gw := NewReal(cfg, store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := gw.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gw.Stop() }()

	ch, err := gw.StreamChat(t.Context(), ChatRequest{
		OpenAIID:        openai.NewID("chatcmpl_"),
		Model:           model,
		ReasoningEffort: "high",
		FinalUser:       openai.ChatMessage{Role: "user", Content: openai.NewTextContent("Think step by step, then answer: what is 17 * 23?")},
	})
	if err != nil {
		t.Fatal(err)
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
			t.Fatalf("stream error: %v", ev.Error)
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
	if os.Getenv("COPILOT_API_LIVE_TESTS") != "1" {
		t.Skip("set COPILOT_API_LIVE_TESTS=1 to run live Copilot integration tests")
	}
	model := os.Getenv("COPILOT_API_LIVE_REASONING_MODEL")
	if model == "" {
		model = "claude-sonnet-4.6"
	}
	root := t.TempDir()
	cfg := config.Config{
		DataDir:        root + "/data",
		StateDir:       root + "/state",
		CacheDir:       root + "/cache",
		ConfigDir:      root + "/config",
		ToolCallTTL:    time.Minute,
		ModelsCacheTTL: time.Minute,
		GitHubToken:    os.Getenv("GITHUB_TOKEN"),
	}
	store := sessionstore.New(cfg.DataDir, cfg.StateDir, cfg.CacheDir)
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	gw := NewReal(cfg, store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := gw.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gw.Stop() }()

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
		t.Fatal(err)
	}
	var first *TurnResult
	for ev := range ch {
		switch ev.Kind {
		case "result":
			first = ev.Result
		case "error":
			t.Fatalf("first stream error: %v", ev.Error)
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
		t.Fatal(err)
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
			t.Fatalf("continuation stream error: %v", ev.Error)
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
