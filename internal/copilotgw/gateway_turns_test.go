package copilotgw

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionfs"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
)

// These tests drive the gateway's public entry points end to end against the
// fake in sdk_fake_test.go. They assert on decisions the gateway makes -
// which SDK call it issues, what session id it mints, what reaches the prompt
// versus the session filesystem, what it persists - rather than on text the
// fake was told to return.

func chatRequest(model, prompt string, history ...openai.ChatMessage) ChatRequest {
	return ChatRequest{
		Model:     model,
		History:   history,
		FinalUser: openai.ChatMessage{Role: "user", Content: openai.NewTextContent(prompt)},
	}
}

func userMessage(text string) openai.ChatMessage {
	return openai.ChatMessage{Role: "user", Content: openai.NewTextContent(text)}
}

func assistantMessage(text string) openai.ChatMessage {
	return openai.ChatMessage{Role: "assistant", Content: openai.NewTextContent(text)}
}

// Chat is stateless per request: it must mint a fresh session id, hydrate the
// conversation so far into that session's filesystem, and send only the final
// user turn as a prompt. Sending the history as prose instead would be a
// different product.
func TestChatHydratesHistoryIntoTheSessionAndSendsOnlyTheFinalTurn(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("answer")}
	gw := newSDKTestGateway(t, runtime)

	result, err := gw.Chat(context.Background(), chatRequest("gpt-test", "what about beta?",
		userMessage("tell me about alpha"), assistantMessage("alpha is a letter")))
	if err != nil {
		t.Fatal(err)
	}

	opens := runtime.openCalls()
	if len(opens) != 1 {
		t.Fatalf("gateway made %d SDK session calls, want 1: %#v", len(opens), opens)
	}
	// Chat never resumes a client-named session: it hydrates a private one and
	// resumes that, so two requests can never share SDK state.
	if opens[0].kind != "resume" {
		t.Fatalf("chat opened the session with %q, want a resume of its own synthetic session", opens[0].kind)
	}
	if !strings.HasPrefix(opens[0].sessionID, "chat_") {
		t.Fatalf("chat session id = %q, want a gateway-minted chat_ id", opens[0].sessionID)
	}
	if opens[0].model != "gpt-test" {
		t.Fatalf("session model = %q, want the requested model", opens[0].model)
	}
	if result.SDKSessionID != opens[0].sessionID {
		t.Fatalf("result SDKSessionID = %q, want the session the gateway opened (%q)", result.SDKSessionID, opens[0].sessionID)
	}

	prompts := runtime.only(t).prompts()
	if len(prompts) != 1 {
		t.Fatalf("gateway sent %d prompts, want 1", len(prompts))
	}
	if prompts[0].Prompt != "what about beta?" {
		t.Fatalf("prompt = %q, want only the final user turn", prompts[0].Prompt)
	}
	if strings.Contains(prompts[0].Prompt, "alpha") {
		t.Fatalf("prompt carried the conversation history as prose: %q", prompts[0].Prompt)
	}

	// The history has to be somewhere, and the whole point of the design is that
	// it is in the session's own event log rather than the prompt.
	if result.RetainedPath == "" {
		t.Fatal("turn result carries no retained session path")
	}
	events, err := os.ReadFile(result.RetainedPath)
	if err != nil {
		t.Fatalf("reading the hydrated session events: %v", err)
	}
	for _, want := range []string{"tell me about alpha", "alpha is a letter"} {
		if !strings.Contains(string(events), want) {
			t.Fatalf("hydrated session events are missing %q:\n%s", want, events)
		}
	}
}

// The finish reason and the session bookkeeping are the gateway's, not the
// runtime's: a plain assistant message followed by idle is a completed turn.
func TestChatPersistsSessionMetadataForTheTurnItRan(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("answer")}
	gw := newSDKTestGateway(t, runtime)

	result, err := gw.Chat(context.Background(), chatRequest("gpt-test", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	if result.FinishReason != "stop" {
		t.Fatalf("finish reason = %q, want stop for a turn that ended on session idle", result.FinishReason)
	}
	if result.Text != "answer" {
		t.Fatalf("text = %q", result.Text)
	}

	meta, err := loadSessionMetadata(gw, result.SDKSessionID)
	if err != nil {
		t.Fatalf("chat did not persist session metadata: %v", err)
	}
	if meta.Kind != "chat" || meta.Model != "gpt-test" || meta.SDKSessionID != result.SDKSessionID {
		t.Fatalf("session metadata = %#v", meta)
	}
	if meta.FinishReason != "stop" {
		t.Fatalf("persisted finish reason = %q, want the turn's", meta.FinishReason)
	}
	if meta.OpenAIID != result.ID {
		t.Fatalf("persisted openai id = %q, want the turn's %q", meta.OpenAIID, result.ID)
	}
}

// A model the catalog does not know must be rejected before any SDK session is
// opened; opening one first would leak a session per bad request.
func TestChatRejectsAnUnknownModelWithoutOpeningASession(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("answer")}
	gw := newSDKTestGateway(t, runtime)

	_, err := gw.Chat(context.Background(), chatRequest("no-such-model", "hi"))
	if err == nil {
		t.Fatal("Chat accepted a model the catalog does not have")
	}
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Kind != apierr.KindNotFound {
		t.Fatalf("error = %#v, want a not-found API error", err)
	}
	if opens := runtime.openCalls(); len(opens) != 0 {
		t.Fatalf("gateway opened %d SDK sessions for an unknown model: %#v", len(opens), opens)
	}
}

// A failed send is an upstream failure, and the turn runner behind it has to be
// wound up rather than left waiting for events that will never arrive.
func TestChatReportsASendFailureAsUpstreamAndReleasesTheTurn(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{sendErr: errors.New("runtime refused the message")}
	gw := newSDKTestGateway(t, runtime)

	_, err := gw.Chat(context.Background(), chatRequest("gpt-test", "hi"))
	if err == nil {
		t.Fatal("Chat succeeded despite the SDK send failing")
	}
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Kind != apierr.KindUpstream {
		t.Fatalf("error = %#v, want an upstream API error", err)
	}
	if !strings.Contains(err.Error(), "runtime refused the message") {
		t.Fatalf("error = %v, want the upstream message preserved", err)
	}
}

// StreamChat returns before the turn finishes and delivers the runtime's
// output as ordered deltas followed by a single terminal result.
func TestStreamChatDeliversOrderedDeltasThenOneResult(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: streamThenAnswerWith("abc")}
	gw := newSDKTestGateway(t, runtime)

	stream, err := gw.StreamChat(context.Background(), chatRequest("gpt-test", "hi"))
	if err != nil {
		t.Fatal(err)
	}
	var deltas strings.Builder
	var result *TurnResult
	var results int
	for event := range stream {
		switch event.Kind {
		case "delta":
			deltas.WriteString(event.Delta)
		case "result":
			results++
			result = event.Result
		case "error":
			t.Fatalf("stream failed: %v", event.Error)
		}
	}
	if deltas.String() != "abc" {
		t.Fatalf("streamed deltas = %q, want the runtime's output in order", deltas.String())
	}
	if results != 1 || result == nil {
		t.Fatalf("stream produced %d results, want exactly 1", results)
	}
	if result.FinishReason != "stop" || result.Text != "abc" {
		t.Fatalf("terminal result = %#v", result)
	}
	// A streaming turn is still a chat turn: the same bookkeeping must land.
	if _, err := loadSessionMetadata(gw, result.SDKSessionID); err != nil {
		t.Fatalf("StreamChat did not persist session metadata: %v", err)
	}
	if opens := runtime.openCalls(); len(opens) != 1 || !opens[0].streaming {
		t.Fatalf("StreamChat opened %#v, want one session configured for streaming", opens)
	}
}

// A first Responses turn creates its own SDK session and persists a record the
// client can later continue from.
func TestCreateResponseCreatesItsOwnSessionAndPersistsTheRecord(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("the answer")}
	gw := newSDKTestGateway(t, runtime)

	result, err := gw.CreateResponse(context.Background(), ResponseRequest{
		Model: "gpt-test",
		Input: openai.PromptContent{Text: "the question"},
		Store: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Response == nil || result.Response.Status != "completed" {
		t.Fatalf("response = %#v, want a completed response", result.Response)
	}
	if result.Response.OutputText != "the answer" {
		t.Fatalf("output text = %q", result.Response.OutputText)
	}

	opens := runtime.openCalls()
	if len(opens) != 1 || opens[0].kind != "create" {
		t.Fatalf("Responses opened %#v, want a single created session", opens)
	}
	if !strings.HasPrefix(opens[0].sessionID, "resp_sdk_") {
		t.Fatalf("responses session id = %q, want a gateway-minted resp_sdk_ id", opens[0].sessionID)
	}

	record, err := gw.store.LoadResponse(result.Response.ID)
	if err != nil {
		t.Fatalf("CreateResponse did not persist its response: %v", err)
	}
	if record.SDKSessionID != opens[0].sessionID {
		t.Fatalf("record session = %q, want the session the turn ran in (%q)", record.SDKSessionID, opens[0].sessionID)
	}
	if record.InputText != "the question" {
		t.Fatalf("record input = %q, want the request's input", record.InputText)
	}
	if record.OutputText != "the answer" || !record.Stored {
		t.Fatalf("record = %#v", record)
	}
	// The input reached the runtime exactly once, as itself.
	prompts := runtime.only(t).prompts()
	if len(prompts) != 1 || prompts[0].Prompt != "the question" {
		t.Fatalf("prompts = %#v", prompts)
	}
}

// The interesting half of prepareResponseTurn: when the SDK cannot resume the
// session a stored response names, the continuation must not fail. The gateway
// mints a fresh session, replays the chain into it, and runs the turn there.
func TestCreateResponseFallsBackToASyntheticSessionWhenResumeFails(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("first")}
	gw := newSDKTestGateway(t, runtime)

	first, err := gw.CreateResponse(context.Background(), ResponseRequest{
		Model: "gpt-test",
		Input: openai.PromptContent{Text: "remember alpha"},
		Store: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	originalSession := runtime.openCalls()[0].sessionID

	// From here the runtime has lost the session the stored response names, which
	// is what a restarted or evicted Copilot session looks like.
	runtime.mu.Lock()
	runtime.resumeErr = refuseResumeOf(originalSession)
	runtime.respond = answerWith("second")
	runtime.mu.Unlock()

	second, err := gw.CreateResponse(context.Background(), ResponseRequest{
		Model:              "gpt-test",
		Input:              openai.PromptContent{Text: "and now beta"},
		PreviousResponseID: first.Response.ID,
		Store:              true,
	})
	if err != nil {
		t.Fatalf("continuation failed instead of falling back: %v", err)
	}
	if second.Response.OutputText != "second" {
		t.Fatalf("continuation output = %q", second.Response.OutputText)
	}

	opens := runtime.openCalls()
	if len(opens) < 3 {
		t.Fatalf("SDK calls = %#v, want the create, a refused resume, then a fallback", opens)
	}
	// Everything between the original create and the last call is the gateway
	// trying to resume the session the record names. It retries once per
	// instruction candidate, so the count is not the interesting part; that they
	// all target the lost session, and that the last call does not, is.
	for i, open := range opens[1 : len(opens)-1] {
		if open.kind != "resume" || open.sessionID != originalSession {
			t.Fatalf("SDK call %d = %#v, want a resume of the stored session %q", i+1, open, originalSession)
		}
	}
	fallback := opens[len(opens)-1]
	if fallback.sessionID == originalSession {
		t.Fatalf("fallback reused the session the runtime had just refused: %#v", opens)
	}
	if !strings.HasPrefix(fallback.sessionID, "resp_sdk_") {
		t.Fatalf("fallback session id = %q, want a freshly minted resp_sdk_ id", fallback.sessionID)
	}

	record, err := gw.store.LoadResponse(second.Response.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.SDKSessionID != fallback.sessionID {
		t.Fatalf("record session = %q, want the fallback session %q", record.SDKSessionID, fallback.sessionID)
	}
	if record.PreviousResponseID != first.Response.ID {
		t.Fatalf("record previous = %q, want the continued response", record.PreviousResponseID)
	}
	// The earlier turn has to survive the session swap somehow: either replayed
	// into the new session's event log, or carried on the prompt.
	prompt := lastPromptFor(t, runtime, fallback.sessionID)
	events := hydratedEventsFor(t, gw, fallback.sessionID)
	if !strings.Contains(prompt, "remember alpha") && !strings.Contains(events, "remember alpha") {
		t.Fatalf("the continued turn was lost by the fallback.\nprompt: %q\nevents: %s", prompt, events)
	}
	if !strings.Contains(prompt, "and now beta") {
		t.Fatalf("fallback prompt = %q, want it to carry this request's own input", prompt)
	}
}

// A streaming Responses turn must produce the same persisted record a
// non-streaming one would, built from the same turn.
func TestStreamResponseStreamsDeltasAndPersistsTheTerminalResponse(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: streamThenAnswerWith("xyz")}
	gw := newSDKTestGateway(t, runtime)

	stream, err := gw.StreamResponse(context.Background(), ResponseRequest{
		Model: "gpt-test",
		Input: openai.PromptContent{Text: "stream please"},
		Store: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var deltas strings.Builder
	var final *openai.Response
	for event := range stream {
		switch event.Kind {
		case "delta":
			if event.ItemID == "" {
				t.Fatal("streamed delta carried no output-item id; only the gateway can assign one")
			}
			deltas.WriteString(event.Delta)
		case "response":
			final = event.Response
		case "error":
			t.Fatalf("stream failed: %v", event.Error)
		}
	}
	if deltas.String() != "xyz" {
		t.Fatalf("streamed deltas = %q", deltas.String())
	}
	if final == nil || final.Status != "completed" || final.OutputText != "xyz" {
		t.Fatalf("terminal response = %#v", final)
	}

	record, err := gw.store.LoadResponse(final.ID)
	if err != nil {
		t.Fatalf("StreamResponse did not persist its response: %v", err)
	}
	if record.OutputText != final.OutputText || record.InputText != "stream please" {
		t.Fatalf("record = %#v, want it to agree with the terminal response", record)
	}
}

// The warm session's whole purpose: the follow-up request must reuse the
// session that was already primed and finally deliver the input that was held
// back, rather than opening a second session and losing it.
func TestStreamResponseReusesTheWarmSessionAndSendsItsHeldInput(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("done")}
	gw := newSDKTestGateway(t, runtime)

	warm, err := gw.WarmResponse(context.Background(), ResponseRequest{
		Model: "gpt-test",
		Input: openai.PromptContent{Text: "held back"},
		Store: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	opensAfterWarm := len(runtime.openCalls())

	stream, err := gw.StreamResponse(context.Background(), ResponseRequest{
		Model:              "gpt-test",
		Input:              openai.PromptContent{Text: "now generate"},
		PreviousResponseID: warm.Response.ID,
		WarmSession:        warm.WarmSession,
		Store:              true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for event := range stream {
		if event.Kind == "error" {
			t.Fatalf("stream failed: %v", event.Error)
		}
	}

	if got := len(runtime.openCalls()); got != opensAfterWarm {
		t.Fatalf("generating on a warm session opened %d more sessions, want 0", got-opensAfterWarm)
	}
	session := runtime.only(t)
	prompts := session.prompts()
	if len(prompts) != 1 {
		t.Fatalf("warm session received %d prompts, want exactly the one generating turn", len(prompts))
	}
	for _, want := range []string{"held back", "now generate"} {
		if !strings.Contains(prompts[0].Prompt, want) {
			t.Fatalf("prompt %q is missing %q; the primed input must be delivered with this turn", prompts[0].Prompt, want)
		}
	}
}

// generate:false primes a session and holds the input back. Sending it would
// defeat the entire feature, so the assertion is that nothing was sent.
func TestWarmResponsePrimesASessionWithoutSendingTheInput(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("must not happen")}
	gw := newSDKTestGateway(t, runtime)

	warm, err := gw.WarmResponse(context.Background(), ResponseRequest{
		Model: "gpt-test",
		Input: openai.PromptContent{Text: "hold this"},
		Store: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer warm.WarmSession.Disconnect()

	if got := runtime.only(t).sendCount(); got != 0 {
		t.Fatalf("WarmResponse sent %d prompts to the runtime, want 0", got)
	}
	if warm.Response.Status != "completed" || len(warm.Response.Output) != 0 || warm.Response.OutputText != "" {
		t.Fatalf("warm response = %#v, want a completed response with no output", warm.Response)
	}
	if opens := runtime.openCalls(); len(opens) != 1 || !opens[0].streaming {
		t.Fatalf("WarmResponse opened %#v, want one streaming session", opens)
	}

	record, err := gw.store.LoadResponse(warm.Response.ID)
	if err != nil {
		t.Fatalf("WarmResponse did not persist its response: %v", err)
	}
	// InputPending is what lets a later resume replay input the SDK never saw.
	if !record.InputPending {
		t.Fatal("warm record did not mark its input as pending; a resume would drop it")
	}
	if record.InputText != "hold this" {
		t.Fatalf("warm record input = %q", record.InputText)
	}
	if record.SDKSessionID != warm.WarmSession.sessionID {
		t.Fatalf("record session = %q, want the warm session %q", record.SDKSessionID, warm.WarmSession.sessionID)
	}
}

// Tool-output continuations have no primed session to hold them, so the
// combination is refused rather than silently degraded.
func TestWarmResponseRejectsToolOutputContinuations(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{}
	gw := newSDKTestGateway(t, runtime)

	_, err := gw.WarmResponse(context.Background(), ResponseRequest{
		Model:       "gpt-test",
		Input:       openai.PromptContent{Text: "hi"},
		ToolOutputs: map[string]toolcatalog.ResponseToolOutput{"call_1": {Kind: toolcatalog.ToolKindFunction, CallID: "call_1", Output: "done"}},
	})
	if err == nil {
		t.Fatal("WarmResponse accepted a tool-output continuation")
	}
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Kind != apierr.KindInvalidInput {
		t.Fatalf("error = %#v, want an invalid-request API error", err)
	}
	if opens := runtime.openCalls(); len(opens) != 0 {
		t.Fatalf("gateway opened %d sessions for a rejected request", len(opens))
	}
}

// The warm session the gateway hands back is one it also owns: shutting the
// gateway down must disconnect it rather than leave it live in the runtime.
func TestGatewayStopDisconnectsWarmSessionsItStillOwns(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{}
	gw := newSDKTestGateway(t, runtime)

	warm, err := gw.WarmResponse(context.Background(), ResponseRequest{
		Model: "gpt-test",
		Input: openai.PromptContent{Text: "hold this"},
	})
	if err != nil {
		t.Fatal(err)
	}
	session := runtime.only(t)
	if got := session.disconnectCount(); got != 0 {
		t.Fatalf("warm session was disconnected %d times before shutdown", got)
	}
	_ = gw.Stop()
	if got := session.disconnectCount(); got == 0 {
		t.Fatal("gateway shutdown left a warm SDK session connected")
	}
	if !warm.WarmSession.Disconnected() {
		t.Fatal("gateway shutdown did not mark the warm session disconnected")
	}
}

// loadSessionMetadata reads back the record SaveSessionMetadata wrote. The
// store has no reader for it, and adding one purely for a test would be the
// wrong trade, so this walks the same layout store.SaveSessionMetadata uses.
func loadSessionMetadata(gw *RealGateway, sessionID string) (sessionstore.SessionMetadata, error) {
	var meta sessionstore.SessionMetadata
	raw, err := os.ReadFile(filepath.Join(gw.cfg.DataDir, "sessions", sessionID, "metadata.json"))
	if err != nil {
		return meta, err
	}
	return meta, json.Unmarshal(raw, &meta)
}

// lastPromptFor returns the final prompt sent to the named session.
func lastPromptFor(t *testing.T, runtime *fakeSDKRuntime, sessionID string) string {
	t.Helper()
	runtime.mu.Lock()
	sessions := append([]*fakeSDKSession(nil), runtime.sessions...)
	runtime.mu.Unlock()
	for _, session := range sessions {
		if session.id != sessionID {
			continue
		}
		prompts := session.prompts()
		if len(prompts) == 0 {
			t.Fatalf("no prompt was sent to session %q", sessionID)
		}
		return prompts[len(prompts)-1].Prompt
	}
	t.Fatalf("session %q was never opened", sessionID)
	return ""
}

// hydratedEventsFor returns the session event log the gateway wrote for a
// session, or "" when it wrote none.
func hydratedEventsFor(t *testing.T, gw *RealGateway, sessionID string) string {
	t.Helper()
	path := filepath.Join(gw.fs.SessionRoot(sessionID), strings.TrimPrefix(sessionfs.SessionStatePath, "/"), "events.jsonl")
	events, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(events)
}
