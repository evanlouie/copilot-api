package copilotgw

import (
	"context"
	"fmt"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
	copilot "github.com/github/copilot-sdk/go"
)

// copilotSession is the slice of an SDK session this gateway actually drives.
//
// It exists so the gateway's turn machinery can be exercised without a live
// Copilot CLI subprocess. *copilot.Session is a concrete struct whose transport
// is an unexported jsonrpc2 client, so a test cannot construct one that does
// anything: every method it has goes through that client. Nothing about the
// call path changes - RealGateway still builds the same SessionConfig and still
// receives events through the same OnEvent callback - only the static type of
// the handle does.
//
// The id is exposed as ID() rather than a SessionID field because
// *copilot.Session already has a field by that name, and a type cannot have
// both. sdkSession below is the adapter that closes that gap.
type copilotSession interface {
	ID() string
	Send(ctx context.Context, options copilot.MessageOptions) (string, error)
	Abort(ctx context.Context) error
	Disconnect() error
}

// sdkSession adapts a real *copilot.Session to copilotSession.
type sdkSession struct{ *copilot.Session }

func (s sdkSession) ID() string { return s.Session.SessionID }

// sdkSessionOpener opens SDK sessions from a session config this gateway has
// already built. It is the seam the fake in the tests plugs into; production
// uses clientSessionOpener, a direct pass-through to *copilot.Client.
type sdkSessionOpener interface {
	CreateSession(ctx context.Context, cfg *copilot.SessionConfig) (copilotSession, error)
	ResumeSession(ctx context.Context, sessionID string, cfg *copilot.ResumeSessionConfig) (copilotSession, error)
}

type clientSessionOpener struct{ client *copilot.Client }

func (c clientSessionOpener) CreateSession(ctx context.Context, cfg *copilot.SessionConfig) (copilotSession, error) {
	return wrapSDKSession(c.client.CreateSession(ctx, cfg))
}

func (c clientSessionOpener) ResumeSession(ctx context.Context, sessionID string, cfg *copilot.ResumeSessionConfig) (copilotSession, error) {
	return wrapSDKSession(c.client.ResumeSession(ctx, sessionID, cfg))
}

// wrapSDKSession keeps "no session and no error" a nil interface. Wrapping a nil
// *copilot.Session would produce a non-nil copilotSession and silently defeat
// every `session == nil` guard the callers rely on.
func wrapSDKSession(s *copilot.Session, err error) (copilotSession, error) {
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, nil
	}
	return sdkSession{s}, nil
}

// sessions resolves the SDK session opener, defaulting to the real client so
// that gateways built as struct literals (which the tests do) still work.
func (g *RealGateway) sessions() sdkSessionOpener {
	if g.sessionOpener != nil {
		return g.sessionOpener
	}
	return clientSessionOpener{client: g.client}
}

func (g *RealGateway) createSession(ctx context.Context, sessionID, model, instructions, reasoning string, rt *toolproxy.RequestTools, streaming bool, events *sessionEventSink) (copilotSession, error) {
	if err := g.fs.EnsureSession(sessionID); err != nil {
		return nil, fmt.Errorf("ensure session fs: %w", err)
	}
	var lastErr error
	for _, candidate := range openai.InstructionCandidates(instructions) {
		cfg := g.newCreateSessionConfig(sessionID, model, candidate, reasoning, rt, streaming, events)
		s, err := g.sessions().CreateSession(ctx, cfg)
		if err == nil {
			return s, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
func (g *RealGateway) resumeSession(ctx context.Context, sessionID, model, instructions, reasoning string, rt *toolproxy.RequestTools, streaming bool, events *sessionEventSink) (copilotSession, error) {
	if err := g.fs.EnsureSession(sessionID); err != nil {
		return nil, fmt.Errorf("ensure session fs: %w", err)
	}
	var lastErr error
	for _, candidate := range openai.InstructionCandidates(instructions) {
		cfg := g.newResumeSessionConfig(model, candidate, reasoning, rt, streaming, events)
		s, err := g.sessions().ResumeSession(ctx, sessionID, cfg)
		if err == nil {
			return s, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
func (g *RealGateway) newCreateSessionConfig(sessionID, model, instructions, reasoning string, rt *toolproxy.RequestTools, streaming bool, events *sessionEventSink) *copilot.SessionConfig {
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
func (g *RealGateway) newResumeSessionConfig(model, instructions, reasoning string, rt *toolproxy.RequestTools, streaming bool, events *sessionEventSink) *copilot.ResumeSessionConfig {
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
	enableConfigDiscovery          *bool
	mcpServers                     map[string]copilot.MCPServerConfig
	skillDirectories               []string
	disabledSkills                 []string
	infiniteSessions               *copilot.InfiniteSessionConfig
	streaming                      *bool
	includeSubAgentStreamingEvents *bool
	onEvent                        copilot.SessionEventHandler
	createSessionFSProvider        func(session *copilot.Session) copilot.SessionFSProvider
	skipCustomInstructions         *bool
	enableHostGitOperations        *bool
	enableSessionStore             *bool
	enableSkills                   *bool
	customAgentsLocalOnly          *bool
	coauthorEnabled                *bool
	manageScheduleEnabled          *bool
}

func (g *RealGateway) sessionRuntimeDefaults(streaming bool, events *sessionEventSink) sessionRuntimeDefaults {
	return sessionRuntimeDefaults{
		workingDirectory:               "/",
		configDirectory:                g.cfg.ConfigDir,
		enableConfigDiscovery:          copilot.Bool(false),
		mcpServers:                     map[string]copilot.MCPServerConfig{},
		skillDirectories:               nil,
		disabledSkills:                 []string{"*"},
		infiniteSessions:               &copilot.InfiniteSessionConfig{Enabled: copilot.Bool(false)},
		streaming:                      copilot.Bool(streaming),
		includeSubAgentStreamingEvents: copilot.Bool(false),
		onEvent:                        events.send,
		createSessionFSProvider:        func(session *copilot.Session) copilot.SessionFSProvider { return g.fs.Provider(session.SessionID) },
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
	cfg.CreateSessionFSProvider = d.createSessionFSProvider
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
	cfg.CreateSessionFSProvider = d.createSessionFSProvider
	cfg.SkipCustomInstructions = d.skipCustomInstructions
	cfg.EnableHostGitOperations = d.enableHostGitOperations
	cfg.EnableSessionStore = d.enableSessionStore
	cfg.EnableSkills = d.enableSkills
	cfg.CustomAgentsLocalOnly = d.customAgentsLocalOnly
	cfg.CoauthorEnabled = d.coauthorEnabled
	cfg.ManageScheduleEnabled = d.manageScheduleEnabled
}
