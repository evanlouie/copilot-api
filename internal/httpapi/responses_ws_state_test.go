package httpapi

import (
	"context"
	"testing"

	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
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

// slotProbeWriter is the connection writer as far as responseSlotTransport can
// tell, and records for every frame whether the connection's response slot was
// already free at the moment that frame went out. That is the only thing the
// protocol actually requires: a client sends its next response.create the
// instant it reads response.completed, so the slot must be free no later than
// the frame it reads.
type slotProbeWriter struct {
	state  *responsesWebSocketState
	frames []slotProbeFrame
}

type slotProbeFrame struct {
	payload  string
	slotFree bool
}

func (w *slotProbeWriter) name() string { return "probe" }

func (w *slotProbeWriter) writePayload(payload []byte) error {
	w.record(payload)
	return nil
}

func (w *slotProbeWriter) writePayloadReleasing(payload []byte, release func()) error {
	if release != nil {
		release()
	}
	w.record(payload)
	return nil
}

func (w *slotProbeWriter) record(payload []byte) {
	w.state.mu.Lock()
	free := !w.state.active
	w.state.mu.Unlock()
	w.frames = append(w.frames, slotProbeFrame{payload: string(payload), slotFree: free})
}

// state.finish() is deferred, so it runs after the terminal frame is already on
// the wire. A client that sends the next response.create the moment it sees
// response.completed - the obvious, correct behaviour - was therefore answered
// with "only one response.create may be active per WebSocket connection".
// Freeing the slot as part of writing the terminal frame closes that window.
func TestWebSocketResponseSlotIsFreeByTheTerminalFrame(t *testing.T) {
	t.Parallel()
	for _, terminal := range []string{"response.completed", "response.failed", "response.incomplete"} {
		t.Run(terminal, func(t *testing.T) {
			t.Parallel()
			state := &responsesWebSocketState{}
			if !state.tryStart() {
				t.Fatal("tryStart refused the first response.create")
			}
			probe := &slotProbeWriter{state: state}
			transport := &responseSlotTransport{writer: probe, release: state.endActive}

			for _, eventType := range []string{"response.created", "response.in_progress", terminal} {
				if err := transport.writeResponseEventPayload(openai.ResponseStreamEvent{Type: eventType}, []byte(eventType)); err != nil {
					t.Fatal(err)
				}
			}

			if len(probe.frames) != 3 {
				t.Fatalf("frames = %#v, want three", probe.frames)
			}
			for _, frame := range probe.frames[:2] {
				if frame.slotFree {
					t.Fatalf("the slot was already free while %s was being written; a second response.create would have been accepted mid-response", frame.payload)
				}
			}
			if !probe.frames[2].slotFree {
				t.Fatalf("the slot was still held while %s was being written; a client that replies to it immediately gets a spurious rejection", probe.frames[2].payload)
			}
			state.finish()
		})
	}
}

// An error envelope ends a response just as finally as a terminal event, so the
// slot has to be free before that frame goes out too. Both writers share this
// ordering helper.
func TestWebSocketWriterReleasesBeforeTheFrameGoesOut(t *testing.T) {
	t.Parallel()
	state := &responsesWebSocketState{}
	if !state.tryStart() {
		t.Fatal("tryStart refused the first response.create")
	}
	writer := &webSocketJSONWriter{}
	slotFreeAtWrite := false
	err := writer.releaseThenWrite(state.endActive, func(context.Context) error {
		state.mu.Lock()
		slotFreeAtWrite = !state.active
		state.mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slotFreeAtWrite {
		t.Fatal("the frame was written while the slot was still held; a client replying to it immediately gets a spurious rejection")
	}
	state.finish()
}
