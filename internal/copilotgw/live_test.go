package copilotgw

import (
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
	gw := NewReal(cfg, store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := gw.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer gw.Stop()
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
	gw := NewReal(cfg, store, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := gw.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer gw.Stop()

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
	gotResult := false
	for ev := range ch {
		switch ev.Kind {
		case "reasoning_delta":
			if ev.Delta != "" {
				sawReasoning = true
			}
		case "delta":
			if ev.Delta != "" && !sawReasoning {
				sawContentBeforeReasoning = true
			}
		case "result":
			gotResult = true
			if ev.Result != nil && ev.Result.Reasoning == "" {
				t.Error("final turn result carried no reasoning text")
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
	if sawContentBeforeReasoning {
		t.Fatal("content delta arrived before any reasoning delta")
	}
}
