package copilotgw

import (
	"context"
	"fmt"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	copilot "github.com/github/copilot-sdk/go"
)

func (g *RealGateway) createSession(ctx context.Context, sessionID, model, instructions, reasoning string, rt *toolproxy.RequestTools, streaming bool, events chan<- copilot.SessionEvent) (*copilot.Session, error) {
	if err := g.fs.EnsureSession(sessionID); err != nil {
		return nil, fmt.Errorf("ensure session fs: %w", err)
	}
	var lastErr error
	for _, candidate := range openai.InstructionCandidates(instructions) {
		cfg := g.newCreateSessionConfig(sessionID, model, candidate, reasoning, rt, streaming, events)
		s, err := g.client.CreateSession(ctx, cfg)
		if err == nil {
			return s, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
func (g *RealGateway) resumeSession(ctx context.Context, sessionID, model, instructions, reasoning string, rt *toolproxy.RequestTools, streaming bool, events chan<- copilot.SessionEvent) (*copilot.Session, error) {
	if err := g.fs.EnsureSession(sessionID); err != nil {
		return nil, fmt.Errorf("ensure session fs: %w", err)
	}
	var lastErr error
	for _, candidate := range openai.InstructionCandidates(instructions) {
		cfg := g.newResumeSessionConfig(model, candidate, reasoning, rt, streaming, events)
		s, err := g.client.ResumeSession(ctx, sessionID, cfg)
		if err == nil {
			return s, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
func (g *RealGateway) newCreateSessionConfig(sessionID, model, instructions, reasoning string, rt *toolproxy.RequestTools, streaming bool, events chan<- copilot.SessionEvent) *copilot.SessionConfig {
	cfg := &copilot.SessionConfig{
		SessionID:           sessionID,
		ClientName:          "copilot-api",
		Model:               model,
		ReasoningEffort:     reasoning,
		Tools:               rt.Tools(),
		AvailableTools:      rt.AvailableTools(),
		SystemMessage:       &copilot.SystemMessageConfig{Mode: "replace", Content: instructions},
		OnPermissionRequest: rt.PermissionHandler(),
	}
	g.sessionRuntimeDefaults(streaming, events).applyCreate(cfg)
	if g.cfg.GitHubToken != "" {
		cfg.GitHubToken = g.cfg.GitHubToken
	}
	return cfg
}
func (g *RealGateway) newResumeSessionConfig(model, instructions, reasoning string, rt *toolproxy.RequestTools, streaming bool, events chan<- copilot.SessionEvent) *copilot.ResumeSessionConfig {
	cfg := &copilot.ResumeSessionConfig{
		ClientName:          "copilot-api",
		Model:               model,
		ReasoningEffort:     reasoning,
		Tools:               rt.Tools(),
		AvailableTools:      rt.AvailableTools(),
		SystemMessage:       &copilot.SystemMessageConfig{Mode: "replace", Content: instructions},
		OnPermissionRequest: rt.PermissionHandler(),
	}
	g.sessionRuntimeDefaults(streaming, events).applyResume(cfg)
	if g.cfg.GitHubToken != "" {
		cfg.GitHubToken = g.cfg.GitHubToken
	}
	return cfg
}

type sessionRuntimeDefaults struct {
	workingDirectory               string
	configDirectory                string
	enableConfigDiscovery          bool
	mcpServers                     map[string]copilot.MCPServerConfig
	skillDirectories               []string
	disabledSkills                 []string
	infiniteSessions               *copilot.InfiniteSessionConfig
	streaming                      *bool
	includeSubAgentStreamingEvents *bool
	onEvent                        copilot.SessionEventHandler
	createSessionFsProvider        func(session *copilot.Session) copilot.SessionFsProvider
	skipCustomInstructions         *bool
	enableHostGitOperations        *bool
	enableSessionStore             *bool
	enableSkills                   *bool
	customAgentsLocalOnly          *bool
	coauthorEnabled                *bool
	manageScheduleEnabled          *bool
}

func (g *RealGateway) sessionRuntimeDefaults(streaming bool, events chan<- copilot.SessionEvent) sessionRuntimeDefaults {
	return sessionRuntimeDefaults{
		workingDirectory:               "/",
		configDirectory:                g.cfg.ConfigDir,
		enableConfigDiscovery:          false,
		mcpServers:                     map[string]copilot.MCPServerConfig{},
		skillDirectories:               nil,
		disabledSkills:                 []string{"*"},
		infiniteSessions:               &copilot.InfiniteSessionConfig{Enabled: copilot.Bool(false)},
		streaming:                      copilot.Bool(streaming),
		includeSubAgentStreamingEvents: copilot.Bool(false),
		onEvent:                        func(e copilot.SessionEvent) { sendEvent(events, e) },
		createSessionFsProvider:        func(session *copilot.Session) copilot.SessionFsProvider { return g.fs.Provider(session.SessionID) },
		skipCustomInstructions:         copilot.Bool(true),
		enableHostGitOperations:        copilot.Bool(false),
		enableSessionStore:             copilot.Bool(false),
		enableSkills:                   copilot.Bool(false),
		customAgentsLocalOnly:          copilot.Bool(true),
		coauthorEnabled:                copilot.Bool(false),
		manageScheduleEnabled:          copilot.Bool(false),
	}
}
func (d sessionRuntimeDefaults) applyCreate(cfg *copilot.SessionConfig) {
	cfg.WorkingDirectory = d.workingDirectory
	cfg.ConfigDirectory = d.configDirectory
	cfg.EnableConfigDiscovery = d.enableConfigDiscovery
	cfg.MCPServers = d.mcpServers
	cfg.SkillDirectories = d.skillDirectories
	cfg.DisabledSkills = d.disabledSkills
	cfg.InfiniteSessions = d.infiniteSessions
	cfg.Streaming = d.streaming
	cfg.IncludeSubAgentStreamingEvents = d.includeSubAgentStreamingEvents
	cfg.OnEvent = d.onEvent
	cfg.CreateSessionFsProvider = d.createSessionFsProvider
	cfg.SkipCustomInstructions = d.skipCustomInstructions
	cfg.EnableHostGitOperations = d.enableHostGitOperations
	cfg.EnableSessionStore = d.enableSessionStore
	cfg.EnableSkills = d.enableSkills
	cfg.CustomAgentsLocalOnly = d.customAgentsLocalOnly
	cfg.CoauthorEnabled = d.coauthorEnabled
	cfg.ManageScheduleEnabled = d.manageScheduleEnabled
}
func (d sessionRuntimeDefaults) applyResume(cfg *copilot.ResumeSessionConfig) {
	cfg.WorkingDirectory = d.workingDirectory
	cfg.ConfigDirectory = d.configDirectory
	cfg.EnableConfigDiscovery = d.enableConfigDiscovery
	cfg.MCPServers = d.mcpServers
	cfg.SkillDirectories = d.skillDirectories
	cfg.DisabledSkills = d.disabledSkills
	cfg.InfiniteSessions = d.infiniteSessions
	cfg.Streaming = d.streaming
	cfg.IncludeSubAgentStreamingEvents = d.includeSubAgentStreamingEvents
	cfg.OnEvent = d.onEvent
	cfg.CreateSessionFsProvider = d.createSessionFsProvider
	cfg.SkipCustomInstructions = d.skipCustomInstructions
	cfg.EnableHostGitOperations = d.enableHostGitOperations
	cfg.EnableSessionStore = d.enableSessionStore
	cfg.EnableSkills = d.enableSkills
	cfg.CustomAgentsLocalOnly = d.customAgentsLocalOnly
	cfg.CoauthorEnabled = d.coauthorEnabled
	cfg.ManageScheduleEnabled = d.manageScheduleEnabled
}
func sendEvent(ch chan<- copilot.SessionEvent, e copilot.SessionEvent) {
	ch <- e
}
