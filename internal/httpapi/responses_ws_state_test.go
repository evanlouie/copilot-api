package httpapi

import (
	"testing"

	"github.com/evanlouie/copilot-api/internal/copilotgw"
)

// A response.create that finishes while the connection is being torn down can
// still install a warm session: the !generate branch of
// handleWebSocketResponseCreate ends in state.replaceWarm(res.WarmSession).
// responsesWebSocket used to run state.close() before state.wait(), so that
// install could land on state that had already been dropped - a live SDK
// session plus its two retention pins parked where nothing in this package
// would ever disconnect them.
//
// The refusal is structural rather than a matter of getting one call ordering
// right, so it holds however the state is driven.
func TestWebSocketStateRefusesAWarmSessionOfferedAfterClose(t *testing.T) {
	t.Parallel()
	state := &responsesWebSocketState{}
	state.close()

	warm := &copilotgw.WarmResponseSession{}
	state.replaceWarm(warm)

	if !warm.Disconnected() {
		t.Fatal("a warm session offered after close was left connected; nothing in httpapi would ever disconnect it")
	}
	state.mu.Lock()
	stored := state.warm
	state.mu.Unlock()
	if stored != nil {
		t.Fatalf("closed state stored warm session %#v", stored)
	}
}

// The teardown responsesWebSocket performs, exercised directly: an in-flight
// response.create installs a warm session and only then finishes, so shutdown
// must wait for it before dropping the state it writes to. The previous order
// - close, then wait - left that session connected.
func TestWebSocketStateShutdownWaitsForInFlightWorkBeforeDroppingIt(t *testing.T) {
	t.Parallel()
	state := &responsesWebSocketState{}
	if !state.tryStart() {
		t.Fatal("tryStart refused the first response.create")
	}
	warm := &copilotgw.WarmResponseSession{}
	running := make(chan struct{})
	go func() {
		defer state.finish()
		close(running)
		state.replaceWarm(warm)
	}()
	<-running

	state.shutdown()

	if !warm.Disconnected() {
		t.Fatal("teardown left an in-flight response.create's warm session connected")
	}
}

// The ordinary case must keep working: a warm session installed while the
// connection is live is stored, replaced, and finally disconnected by close.
func TestWebSocketStateStoresAndReplacesWarmSessionsWhileOpen(t *testing.T) {
	t.Parallel()
	state := &responsesWebSocketState{}
	first := &copilotgw.WarmResponseSession{}
	second := &copilotgw.WarmResponseSession{}

	state.replaceWarm(first)
	state.replaceWarm(second)
	if !first.Disconnected() {
		t.Fatal("replaceWarm left the session it replaced connected")
	}
	if second.Disconnected() {
		t.Fatal("replaceWarm disconnected the session it was installing")
	}

	state.close()
	if !second.Disconnected() {
		t.Fatal("close left the stored warm session connected")
	}
}
