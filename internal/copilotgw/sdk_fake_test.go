package copilotgw

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	copilot "github.com/github/copilot-sdk/go"
)

// This file is the fake that sits at the gateway's SDK boundary, where the
// hand-written fakes in internal/httpapi cannot reach: those stub the whole
// Gateway, so nothing in chat.go, responses.go, response_session.go or
// warm_response.go runs under them at all.
//
// A fake here is the opposite: the real RealGateway runs, with its real model
// cache, prompt resolution, session filesystem, tool broker, turn runner and
// sessionstore. Only the Copilot runtime is replaced, because *copilot.Session
// reaches it through an unexported jsonrpc2 client that no test can supply.

// fakeSDKRuntime is the Copilot runtime as far as the gateway can tell.
type fakeSDKRuntime struct {
	mu       sync.Mutex
	opens    []sdkOpen
	sessions []*fakeSDKSession

	// createErr fails session creation. resumeErr, given a session id, decides
	// whether the runtime will resume it: a real runtime refuses only the
	// sessions it has lost, which is what drives the gateway's
	// synthetic-continuation fallback.
	createErr error
	resumeErr func(sessionID string) error
	// sendErr fails session.send.
	sendErr error
	// respond is what the runtime does when the gateway sends a prompt. It runs
	// on the calling goroutine, exactly like an SDK that answered instantly.
	respond func(*fakeSDKSession, copilot.MessageOptions)
}

// sdkOpen is one session the gateway asked the runtime for, with the parts of
// the session config worth asserting on.
type sdkOpen struct {
	kind            string // "create" or "resume"
	sessionID       string
	model           string
	reasoningEffort string
	instructions    string
	streaming       bool
	toolNames       []string
}

func (f *fakeSDKRuntime) CreateSession(_ context.Context, cfg *copilot.SessionConfig) (copilotSession, error) {
	open := sdkOpen{kind: "create", sessionID: cfg.SessionID, model: cfg.Model, reasoningEffort: cfg.ReasoningEffort, streaming: cfg.Streaming != nil && *cfg.Streaming}
	if cfg.SystemMessage != nil {
		open.instructions = cfg.SystemMessage.Content
	}
	for _, tool := range cfg.Tools {
		open.toolNames = append(open.toolNames, tool.Name)
	}
	return f.open(open, cfg.OnEvent, f.createErr)
}

func (f *fakeSDKRuntime) ResumeSession(_ context.Context, sessionID string, cfg *copilot.ResumeSessionConfig) (copilotSession, error) {
	open := sdkOpen{kind: "resume", sessionID: sessionID, model: cfg.Model, reasoningEffort: cfg.ReasoningEffort, streaming: cfg.Streaming != nil && *cfg.Streaming}
	if cfg.SystemMessage != nil {
		open.instructions = cfg.SystemMessage.Content
	}
	for _, tool := range cfg.Tools {
		open.toolNames = append(open.toolNames, tool.Name)
	}
	var err error
	f.mu.Lock()
	if f.resumeErr != nil {
		err = f.resumeErr(sessionID)
	}
	f.mu.Unlock()
	return f.open(open, cfg.OnEvent, err)
}

// refuseResumeOf makes the runtime refuse exactly the named sessions, the way a
// runtime that lost or evicted them would.
func refuseResumeOf(ids ...string) func(string) error {
	lost := map[string]struct{}{}
	for _, id := range ids {
		lost[id] = struct{}{}
	}
	return func(sessionID string) error {
		if _, ok := lost[sessionID]; ok {
			return errors.New("no such session: " + sessionID)
		}
		return nil
	}
}

func (f *fakeSDKRuntime) open(open sdkOpen, onEvent copilot.SessionEventHandler, err error) (copilotSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens = append(f.opens, open)
	if err != nil {
		return nil, err
	}
	session := &fakeSDKSession{runtime: f, id: open.sessionID, onEvent: onEvent}
	f.sessions = append(f.sessions, session)
	return session, nil
}

// openCalls returns every session the gateway asked for, in order.
func (f *fakeSDKRuntime) openCalls() []sdkOpen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sdkOpen(nil), f.opens...)
}

// only returns the single session the gateway opened, failing if there is not
// exactly one. Tests that expect a single session say so through it.
func (f *fakeSDKRuntime) only(t *testing.T) *fakeSDKSession {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sessions) != 1 {
		t.Fatalf("gateway opened %d SDK sessions, want exactly 1", len(f.sessions))
	}
	return f.sessions[0]
}

type fakeSDKSession struct {
	runtime *fakeSDKRuntime
	id      string
	onEvent copilot.SessionEventHandler

	mu          sync.Mutex
	sent        []copilot.MessageOptions
	aborts      int
	disconnects int
}

func (s *fakeSDKSession) ID() string { return s.id }

func (s *fakeSDKSession) Send(_ context.Context, options copilot.MessageOptions) (string, error) {
	s.mu.Lock()
	s.sent = append(s.sent, options)
	s.mu.Unlock()
	s.runtime.mu.Lock()
	sendErr, respond := s.runtime.sendErr, s.runtime.respond
	s.runtime.mu.Unlock()
	if sendErr != nil {
		return "", sendErr
	}
	if respond != nil {
		respond(s, options)
	}
	return "msg_" + s.id, nil
}

func (s *fakeSDKSession) Abort(context.Context) error {
	s.mu.Lock()
	s.aborts++
	s.mu.Unlock()
	return nil
}

func (s *fakeSDKSession) Disconnect() error {
	s.mu.Lock()
	s.disconnects++
	s.mu.Unlock()
	return nil
}

// prompts returns every prompt the gateway sent to this session.
func (s *fakeSDKSession) prompts() []copilot.MessageOptions {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]copilot.MessageOptions(nil), s.sent...)
}

func (s *fakeSDKSession) sendCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

func (s *fakeSDKSession) disconnectCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.disconnects
}

// emit delivers events the way the runtime does: through the OnEvent callback
// the gateway installed on this session's config.
func (s *fakeSDKSession) emit(events ...copilot.SessionEvent) {
	if s.onEvent == nil {
		return
	}
	for _, event := range events {
		s.onEvent(event)
	}
}

// answerWith is the ordinary runtime behaviour: one assistant message, then
// idle.
func answerWith(text string) func(*fakeSDKSession, copilot.MessageOptions) {
	return func(s *fakeSDKSession, _ copilot.MessageOptions) {
		s.emit(
			copilot.SessionEvent{Data: &copilot.AssistantMessageData{MessageID: "msg_1", Content: text}},
			copilot.SessionEvent{Data: &copilot.SessionIdleData{}},
		)
	}
}

// streamThenAnswerWith streams the text a character at a time before the final
// message, which is what a streaming turn looks like on the wire.
func streamThenAnswerWith(text string) func(*fakeSDKSession, copilot.MessageOptions) {
	return func(s *fakeSDKSession, _ copilot.MessageOptions) {
		s.emit(copilot.SessionEvent{Data: &copilot.AssistantTurnStartData{}})
		for _, r := range text {
			s.emit(copilot.SessionEvent{Data: &copilot.AssistantMessageDeltaData{MessageID: "msg_1", DeltaContent: string(r)}})
		}
		s.emit(
			copilot.SessionEvent{Data: &copilot.AssistantMessageData{MessageID: "msg_1", Content: text}},
			copilot.SessionEvent{Data: &copilot.SessionIdleData{}},
		)
	}
}

// newSDKTestGateway builds a real RealGateway through its real constructor with
// only the SDK boundary replaced, so everything the gateway does between the
// HTTP layer and the runtime is genuinely executed.
func newSDKTestGateway(t *testing.T, runtime *fakeSDKRuntime, models ...Model) *RealGateway {
	t.Helper()
	if len(models) == 0 {
		models = []Model{{ID: "gpt-test"}}
	}
	root := t.TempDir()
	cfg := config.Config{
		DataDir:        filepath.Join(root, "data"),
		StateDir:       filepath.Join(root, "state"),
		CacheDir:       filepath.Join(root, "cache"),
		ConfigDir:      filepath.Join(root, "config"),
		ToolCallTTL:    time.Minute,
		ModelsCacheTTL: time.Hour,
	}
	store := sessionstore.New(cfg.DataDir, cfg.StateDir, cfg.CacheDir)
	if err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	gw := NewReal(cfg, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	gw.sessionOpener = runtime
	gw.modelsFetcher = func(context.Context) ([]Model, error) {
		return append([]Model(nil), models...), nil
	}
	return gw
}
