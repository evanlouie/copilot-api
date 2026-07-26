package copilotgw

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolproxy"
)

// A request that reaches newTurnRunner after Stop has closed the runner
// registry has no loop behind it: loop is the sole producer on r.updates and
// the sole owner of closeStreams. A runner handed back without one leaves
// waitInitial and the response stream channel waiting on a producer that will
// never exist, and go runner.discardInitial parked on <-r.updates forever.
//
// Every entry point must therefore fail such a request outright, with the same
// 503 WarmResponse already returns for the equivalent registry-after-close race.
func TestStreamResponseAfterStopFailsInsteadOfHanging(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("ok")}
	gw := newSDKTestGateway(t, runtime)
	if err := gw.Stop(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := gw.StreamResponse(ctx, ResponseRequest{Model: "gpt-test", ResponseID: "resp_stream", Input: openai.PromptContent{Text: "hi"}})
	if err == nil {
		select {
		case event, ok := <-ch:
			t.Fatalf("StreamResponse after Stop produced event %#v (open=%t) from a runner with no loop", event, ok)
		case <-ctx.Done():
			t.Fatal("StreamResponse after Stop returned a channel that produced nothing and was never closed")
		}
	}
	assertGatewayShuttingDown(t, err)
	if session := onlySessionDisconnects(t, runtime); session != 1 {
		t.Fatalf("SDK session disconnects = %d, want 1; the refused turn must not leave a session connected", session)
	}
}

func TestCreateResponseAfterStopFailsInsteadOfHanging(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("ok")}
	gw := newSDKTestGateway(t, runtime)
	if err := gw.Stop(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := gw.CreateResponse(ctx, ResponseRequest{Model: "gpt-test", ResponseID: "resp_create", Input: openai.PromptContent{Text: "hi"}})
	if err == nil {
		t.Fatal("CreateResponse after Stop succeeded")
	}
	if errors.Is(err, context.DeadlineExceeded) || kindOf(err) == apierr.KindTimeout {
		t.Fatalf("CreateResponse after Stop blocked until the request context expired: %v", err)
	}
	assertGatewayShuttingDown(t, err)
}

func TestStreamChatAfterStopFailsInsteadOfHanging(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("ok")}
	gw := newSDKTestGateway(t, runtime)
	if err := gw.Stop(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ch, err := gw.StreamChat(ctx, chatRequest("gpt-test", "hi"))
	if err == nil {
		select {
		case event, ok := <-ch:
			t.Fatalf("StreamChat after Stop produced event %#v (open=%t) from a runner with no loop", event, ok)
		case <-ctx.Done():
			t.Fatal("StreamChat after Stop returned a channel that produced nothing and was never closed")
		}
	}
	assertGatewayShuttingDown(t, err)
}

func TestChatAfterStopFailsInsteadOfHanging(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("ok")}
	gw := newSDKTestGateway(t, runtime)
	if err := gw.Stop(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, err := gw.Chat(ctx, chatRequest("gpt-test", "hi"))
	if err == nil {
		t.Fatal("Chat after Stop succeeded")
	}
	if errors.Is(err, context.DeadlineExceeded) || kindOf(err) == apierr.KindTimeout {
		t.Fatalf("Chat after Stop blocked until the request context expired: %v", err)
	}
	assertGatewayShuttingDown(t, err)
}

// A warm session handed to a generating request during Stop must end up owned
// by exactly one of the two registries. Closing warm before active is what
// guarantees it: use is refused once the warm registry is closed, so the
// session stays where Stop's own snapshot will disconnect it.
func TestWarmSessionUseIsRefusedOnceTheRegistryIsClosed(t *testing.T) {
	t.Parallel()
	store := sessionstore.New(t.TempDir(), t.TempDir())
	gw := NewReal(config.Config{ToolCallTTL: time.Minute}, store, nil)
	var releases atomic.Int32
	session := &fakeSDKSession{id: "resp_sdk_warm"}
	warm := &WarmResponseSession{
		responseID:  "resp_warm",
		sessionID:   "resp_sdk_warm",
		model:       "gpt-5",
		session:     session,
		rt:          &toolproxy.RequestTools{},
		pinReleases: []func(){func() { releases.Add(1) }},
	}
	if !gw.trackWarmSession(warm) {
		t.Fatal("gateway refused to track a warm session before Stop")
	}
	// Close the warm registry the way Stop does, without draining it, so use
	// runs in exactly the window Stop used to leave open.
	gw.warm.mu.Lock()
	gw.warm.closed = true
	gw.warm.mu.Unlock()

	req := ResponseRequest{Model: "gpt-5", PreviousResponseID: "resp_warm"}
	if _, ok := warm.use(&req); ok {
		t.Fatal("use handed the session away after the warm registry closed; neither registry would own it")
	}
	if !gw.warm.tracked(warm) {
		t.Fatal("a refused use deregistered the session anyway")
	}
	if got := releases.Load(); got != 0 {
		t.Fatalf("a refused use released %d pins, want 0", got)
	}
	// The session is still the gateway's, so Stop still tears it down.
	if err := gw.Stop(); err != nil {
		t.Fatal(err)
	}
	if !warm.Disconnected() || releases.Load() != 1 {
		t.Fatalf("Stop left the refused session behind: disconnected=%t releases=%d", warm.Disconnected(), releases.Load())
	}
}

func assertGatewayShuttingDown(t *testing.T, err error) {
	t.Helper()
	if kind := kindOf(err); kind != apierr.KindUnavailable {
		t.Fatalf("error = %v (kind %q), want %q", err, kind, apierr.KindUnavailable)
	}
}

func kindOf(err error) apierr.Kind {
	var apiErr *apierr.Error
	if errors.As(err, &apiErr) {
		return apiErr.Kind
	}
	return ""
}

// onlySessionDisconnects reports how many times the single SDK session the
// gateway opened was disconnected.
func onlySessionDisconnects(t *testing.T, runtime *fakeSDKRuntime) int {
	t.Helper()
	return runtime.only(t).disconnectCount()
}
