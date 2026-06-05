package copilotgw

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/sessionfs"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

func TestModelMetadataIncludesTokenLimits(t *testing.T) {
	contextWindow := int64(200000)
	prompt := int64(180000)
	output := int64(8192)

	meta := modelMetadata("GPT 5", true, true, &TokenLimits{
		MaxContextWindowTokens: &contextWindow,
		MaxPromptTokens:        &prompt,
		MaxOutputTokens:        &output,
	}, nil)

	if got := meta["max_context_window_tokens"]; got != contextWindow {
		t.Fatalf("metadata max_context_window_tokens = %#v, want %d", got, contextWindow)
	}
	if got := meta["max_prompt_tokens"]; got != prompt {
		t.Fatalf("metadata max_prompt_tokens = %#v, want %d", got, prompt)
	}
	if got := meta["max_output_tokens"]; got != output {
		t.Fatalf("metadata max_output_tokens = %#v, want %d", got, output)
	}

	capabilities, ok := meta["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("metadata capabilities = %#v, want object", meta["capabilities"])
	}
	limits, ok := capabilities["limits"].(map[string]any)
	if !ok {
		t.Fatalf("metadata capabilities.limits = %#v, want object", capabilities["limits"])
	}
	if got := limits["max_context_window_tokens"]; got != contextWindow {
		t.Fatalf("metadata capabilities.limits.max_context_window_tokens = %#v, want %d", got, contextWindow)
	}
}

func TestRPCTokenLimits(t *testing.T) {
	contextWindow := int64(128000)
	prompt := int64(120000)
	output := int64(4096)

	limits := rpcTokenLimits(&rpc.ModelCapabilitiesLimits{
		MaxContextWindowTokens: &contextWindow,
		MaxPromptTokens:        &prompt,
		MaxOutputTokens:        &output,
	})
	if limits == nil {
		t.Fatal("expected token limits")
	}
	if *limits.MaxContextWindowTokens != contextWindow {
		t.Fatalf("MaxContextWindowTokens = %d, want %d", *limits.MaxContextWindowTokens, contextWindow)
	}
	if *limits.MaxPromptTokens != prompt {
		t.Fatalf("MaxPromptTokens = %d, want %d", *limits.MaxPromptTokens, prompt)
	}
	if *limits.MaxOutputTokens != output {
		t.Fatalf("MaxOutputTokens = %d, want %d", *limits.MaxOutputTokens, output)
	}
}

func TestSDKTokenLimits(t *testing.T) {
	prompt := 120000

	limits := sdkTokenLimits(copilot.ModelLimits{
		MaxContextWindowTokens: copilot.Int(128000),
		MaxPromptTokens:        &prompt,
	})
	if limits == nil {
		t.Fatal("expected token limits")
	}
	if *limits.MaxContextWindowTokens != 128000 {
		t.Fatalf("MaxContextWindowTokens = %d, want 128000", *limits.MaxContextWindowTokens)
	}
	if *limits.MaxPromptTokens != int64(prompt) {
		t.Fatalf("MaxPromptTokens = %d, want %d", *limits.MaxPromptTokens, prompt)
	}
	if limits.MaxOutputTokens != nil {
		t.Fatalf("MaxOutputTokens = %d, want nil", *limits.MaxOutputTokens)
	}
}

func TestSDKTokenLimitsNilContextWindow(t *testing.T) {
	prompt := 8192

	limits := sdkTokenLimits(copilot.ModelLimits{
		MaxContextWindowTokens: nil,
		MaxPromptTokens:        &prompt,
	})
	if limits == nil {
		t.Fatal("expected token limits when MaxPromptTokens is set")
	}
	if limits.MaxContextWindowTokens != nil {
		t.Fatalf("MaxContextWindowTokens = %d, want nil", *limits.MaxContextWindowTokens)
	}
	if limits.MaxPromptTokens == nil || *limits.MaxPromptTokens != int64(prompt) {
		t.Fatalf("MaxPromptTokens = %v, want %d", limits.MaxPromptTokens, prompt)
	}
}

func TestSDKTokenLimitsZeroContextWindowSuppressed(t *testing.T) {
	// A pointer to zero must be treated as "unknown" for context-window
	// limits, matching the pre-v1.0.0 SDK semantics where the field was a
	// plain int and v <= 0 was suppressed.
	limits := sdkTokenLimits(copilot.ModelLimits{
		MaxContextWindowTokens: copilot.Int(0),
		MaxPromptTokens:        nil,
	})
	if limits != nil {
		t.Fatalf("expected nil token limits, got %#v", limits)
	}
}

func TestSDKTokenLimitsAllNil(t *testing.T) {
	if got := sdkTokenLimits(copilot.ModelLimits{}); got != nil {
		t.Fatalf("expected nil token limits when all fields are nil, got %#v", got)
	}
}

func TestEffectiveReasoningEffortUsesExplicitRequest(t *testing.T) {
	gw := cachedModelGateway(Model{
		ID:                        "gpt-5",
		ReasoningEffortKnown:      true,
		SupportsReasoningEffort:   false,
		SupportedReasoningEfforts: []string{"low", "medium"},
	})
	gw.cfg = config.Config{DefaultReasoningEffort: "low"}

	got, err := gw.effectiveReasoningEffort(context.Background(), "gpt-5", " HIGH ", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "high" {
		t.Fatalf("effective reasoning effort = %q, want explicit high", got)
	}
}

func TestEffectiveReasoningEffortUsesClosestSupportedDefault(t *testing.T) {
	tests := []struct {
		name      string
		def       string
		supported []string
		want      string
	}{
		{name: "same effort", def: "medium", supported: []string{"low", "medium", "high"}, want: "medium"},
		{name: "none exact supported", def: "none", supported: []string{"none", "low", "medium", "high"}, want: "none"},
		{name: "rounds high down", def: "high", supported: []string{"low", "medium"}, want: "medium"},
		{name: "rounds low up", def: "low", supported: []string{"medium", "high"}, want: "medium"},
		{name: "tie uses lower effort", def: "medium", supported: []string{"low", "high"}, want: "low"},
		{name: "xhigh rounds down", def: "xhigh", supported: []string{"medium", "high"}, want: "high"},
		{name: "minimal rounds up", def: "minimal", supported: []string{"low", "medium"}, want: "low"},
		{name: "none rounds up when unsupported", def: "none", supported: []string{"low", "medium", "high"}, want: "low"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gw := cachedModelGateway(Model{
				ID:                        "gpt-5",
				ReasoningEffortKnown:      true,
				SupportsReasoningEffort:   true,
				SupportedReasoningEfforts: tc.supported,
			})
			gw.cfg = config.Config{DefaultReasoningEffort: tc.def}

			got, err := gw.effectiveReasoningEffort(context.Background(), "gpt-5", "", "")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("effective reasoning effort = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEffectiveReasoningEffortOmitsDefaultWhenUnsupported(t *testing.T) {
	gw := cachedModelGateway(Model{
		ID:                      "gpt-4.1",
		ReasoningEffortKnown:    true,
		SupportsReasoningEffort: false,
	})
	gw.cfg = config.Config{DefaultReasoningEffort: "high"}

	got, err := gw.effectiveReasoningEffort(context.Background(), "gpt-4.1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("effective reasoning effort = %q, want omitted", got)
	}
}

func TestEffectiveReasoningEffortUsesDefaultWhenSupportKnownWithoutEffortList(t *testing.T) {
	gw := cachedModelGateway(Model{
		ID:                      "gpt-5",
		ReasoningEffortKnown:    true,
		SupportsReasoningEffort: true,
	})
	gw.cfg = config.Config{DefaultReasoningEffort: "high"}

	got, err := gw.effectiveReasoningEffort(context.Background(), "gpt-5", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "high" {
		t.Fatalf("effective reasoning effort = %q, want high", got)
	}
}

func TestEffectiveReasoningEffortUsesModelDefaultWhenSupportListMissing(t *testing.T) {
	gw := cachedModelGateway(Model{
		ID:                      "claude-opus-4.8",
		ReasoningEffortKnown:    true,
		SupportsReasoningEffort: true,
		DefaultReasoningEffort:  "medium",
	})
	gw.cfg = config.Config{DefaultReasoningEffort: "minimal"}

	got, err := gw.effectiveReasoningEffort(context.Background(), "claude-opus-4.8", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "medium" {
		t.Fatalf("effective reasoning effort = %q, want medium", got)
	}
}

func TestEffectiveReasoningEffortNoneOmitsModelDefaultWhenSupportListMissing(t *testing.T) {
	gw := cachedModelGateway(Model{
		ID:                      "claude-opus-4.8",
		ReasoningEffortKnown:    true,
		SupportsReasoningEffort: true,
		DefaultReasoningEffort:  "medium",
	})
	gw.cfg = config.Config{DefaultReasoningEffort: "none"}

	got, err := gw.effectiveReasoningEffort(context.Background(), "claude-opus-4.8", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("effective reasoning effort = %q, want omitted", got)
	}
}

func TestRealClientOptionsUseV1ModeEmpty(t *testing.T) {
	cfg := config.Config{
		CLIPath:     "/tmp/copilot",
		StateDir:    "/tmp/state",
		ConfigDir:   "/tmp/config",
		GitHubToken: "gho_test",
	}
	opts := newRealClientOptions(cfg)
	if opts.Mode != copilot.ModeEmpty {
		t.Fatalf("Mode = %q, want %q", opts.Mode, copilot.ModeEmpty)
	}
	conn, ok := opts.Connection.(copilot.StdioConnection)
	if !ok {
		t.Fatalf("Connection = %T, want copilot.StdioConnection", opts.Connection)
	}
	if conn.Path != cfg.CLIPath {
		t.Fatalf("Connection.Path = %q, want %q", conn.Path, cfg.CLIPath)
	}
	if opts.WorkingDirectory != cfg.StateDir {
		t.Fatalf("WorkingDirectory = %q, want %q", opts.WorkingDirectory, cfg.StateDir)
	}
	if opts.BaseDirectory != cfg.ConfigDir {
		t.Fatalf("BaseDirectory = %q, want %q", opts.BaseDirectory, cfg.ConfigDir)
	}
	if opts.SessionFS == nil {
		t.Fatal("SessionFS is nil")
	}
}

func TestSessionConfigBuildersApplyV1Hardening(t *testing.T) {
	rt, err := toolproxy.NewRequestTools(toolproxy.NewBroker(time.Minute), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	gw := &RealGateway{
		cfg: config.Config{
			ConfigDir:   "/tmp/config",
			GitHubToken: "gho_test",
		},
		fs: sessionfs.NewManager(t.TempDir()),
	}

	createCfg := gw.newCreateSessionConfig("session-id", "gpt-5", "instructions", "medium", rt, true, nil)
	if createCfg.SessionID != "session-id" {
		t.Fatalf("SessionID = %q, want session-id", createCfg.SessionID)
	}
	if createCfg.GitHubToken != "gho_test" {
		t.Fatalf("GitHubToken = %q, want gho_test", createCfg.GitHubToken)
	}
	assertCreateSessionHardening(t, createCfg)

	resumeCfg := gw.newResumeSessionConfig("gpt-5", "instructions", "medium", rt, false, nil)
	if resumeCfg.GitHubToken != "gho_test" {
		t.Fatalf("GitHubToken = %q, want gho_test", resumeCfg.GitHubToken)
	}
	assertResumeSessionHardening(t, resumeCfg)
}

func assertCreateSessionHardening(t *testing.T, cfg *copilot.SessionConfig) {
	t.Helper()
	if len(cfg.AvailableTools) == 0 {
		t.Fatal("AvailableTools is empty")
	}
	if cfg.WorkingDirectory != "/" {
		t.Fatalf("WorkingDirectory = %q, want /", cfg.WorkingDirectory)
	}
	if cfg.ConfigDirectory != "/tmp/config" {
		t.Fatalf("ConfigDirectory = %q, want /tmp/config", cfg.ConfigDirectory)
	}
	assertFalsePtr(t, "EnableConfigDiscovery", cfg.EnableConfigDiscovery)
	if cfg.MCPServers == nil || len(cfg.MCPServers) != 0 {
		t.Fatalf("MCPServers = %#v, want non-nil empty map", cfg.MCPServers)
	}
	if len(cfg.DisabledSkills) != 1 || cfg.DisabledSkills[0] != "*" {
		t.Fatalf("DisabledSkills = %#v, want [*]", cfg.DisabledSkills)
	}
	assertFalsePtr(t, "InfiniteSessions.Enabled", cfg.InfiniteSessions.Enabled)
	assertBoolPtr(t, "Streaming", cfg.Streaming, true)
	assertFalsePtr(t, "IncludeSubAgentStreamingEvents", cfg.IncludeSubAgentStreamingEvents)
	assertTruePtr(t, "SkipCustomInstructions", cfg.SkipCustomInstructions)
	assertFalsePtr(t, "EnableHostGitOperations", cfg.EnableHostGitOperations)
	assertFalsePtr(t, "EnableSessionStore", cfg.EnableSessionStore)
	assertFalsePtr(t, "EnableSkills", cfg.EnableSkills)
	assertTruePtr(t, "CustomAgentsLocalOnly", cfg.CustomAgentsLocalOnly)
	assertFalsePtr(t, "CoauthorEnabled", cfg.CoauthorEnabled)
	assertFalsePtr(t, "ManageScheduleEnabled", cfg.ManageScheduleEnabled)
	if cfg.OnEvent == nil {
		t.Fatal("OnEvent is nil")
	}
	if cfg.CreateSessionFSProvider == nil {
		t.Fatal("CreateSessionFSProvider is nil")
	}
}

func assertResumeSessionHardening(t *testing.T, cfg *copilot.ResumeSessionConfig) {
	t.Helper()
	if len(cfg.AvailableTools) == 0 {
		t.Fatal("AvailableTools is empty")
	}
	if cfg.WorkingDirectory != "/" {
		t.Fatalf("WorkingDirectory = %q, want /", cfg.WorkingDirectory)
	}
	if cfg.ConfigDirectory != "/tmp/config" {
		t.Fatalf("ConfigDirectory = %q, want /tmp/config", cfg.ConfigDirectory)
	}
	assertFalsePtr(t, "EnableConfigDiscovery", cfg.EnableConfigDiscovery)
	if cfg.MCPServers == nil || len(cfg.MCPServers) != 0 {
		t.Fatalf("MCPServers = %#v, want non-nil empty map", cfg.MCPServers)
	}
	if len(cfg.DisabledSkills) != 1 || cfg.DisabledSkills[0] != "*" {
		t.Fatalf("DisabledSkills = %#v, want [*]", cfg.DisabledSkills)
	}
	assertFalsePtr(t, "InfiniteSessions.Enabled", cfg.InfiniteSessions.Enabled)
	assertBoolPtr(t, "Streaming", cfg.Streaming, false)
	assertFalsePtr(t, "IncludeSubAgentStreamingEvents", cfg.IncludeSubAgentStreamingEvents)
	assertTruePtr(t, "SkipCustomInstructions", cfg.SkipCustomInstructions)
	assertFalsePtr(t, "EnableHostGitOperations", cfg.EnableHostGitOperations)
	assertFalsePtr(t, "EnableSessionStore", cfg.EnableSessionStore)
	assertFalsePtr(t, "EnableSkills", cfg.EnableSkills)
	assertTruePtr(t, "CustomAgentsLocalOnly", cfg.CustomAgentsLocalOnly)
	assertFalsePtr(t, "CoauthorEnabled", cfg.CoauthorEnabled)
	assertFalsePtr(t, "ManageScheduleEnabled", cfg.ManageScheduleEnabled)
	if cfg.OnEvent == nil {
		t.Fatal("OnEvent is nil")
	}
	if cfg.CreateSessionFSProvider == nil {
		t.Fatal("CreateSessionFSProvider is nil")
	}
}

func assertTruePtr(t *testing.T, name string, got *bool) {
	t.Helper()
	assertBoolPtr(t, name, got, true)
}

func assertFalsePtr(t *testing.T, name string, got *bool) {
	t.Helper()
	assertBoolPtr(t, name, got, false)
}

func assertBoolPtr(t *testing.T, name string, got *bool, want bool) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is nil, want %v", name, want)
	}
	if *got != want {
		t.Fatalf("%s = %v, want %v", name, *got, want)
	}
}

func TestNewRealNormalizesNilLogger(t *testing.T) {
	store := sessionstore.New(t.TempDir(), t.TempDir(), t.TempDir())
	gw := NewReal(config.Config{}, store, nil)
	if gw.log == nil {
		t.Fatal("NewReal left a nil logger; the constructor must install a discard logger")
	}
	// The installed fallback logger must be safe to use without panicking.
	gw.log.Warn("nil-logger fallback smoke test", "ok", true)
}

func TestRefreshModelsDeduplicatesConcurrentForcedRefreshes(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	gw := &RealGateway{
		modelsCacheTTL: time.Hour,
		modelsFetcher: func(context.Context) ([]Model, error) {
			atomic.AddInt32(&calls, 1)
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
			return []Model{{ID: "gpt-5"}}, nil
		},
	}
	const n = 8
	errs := make(chan error, n)
	for range n {
		go func() {
			_, err := gw.refreshModels(context.Background(), true)
			errs <- err
		}()
	}
	// The first caller is now parked inside the single in-flight fetch.
	<-started
	// Give the remaining callers time to join that fetch rather than starting
	// their own.
	time.Sleep(50 * time.Millisecond)
	close(release)
	for range n {
		if err := <-errs; err != nil {
			t.Fatalf("forced refresh returned error: %v", err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream fetch ran %d times, want 1 (deduplicated)", got)
	}
}
