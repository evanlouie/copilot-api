package copilotgw

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"copilot-api/internal/config"
	"copilot-api/internal/openai"
	"copilot-api/internal/sessionstore"
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
