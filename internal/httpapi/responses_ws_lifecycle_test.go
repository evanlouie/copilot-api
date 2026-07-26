package httpapi

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"go.uber.org/goleak"
)

// A client that vanishes without a close handshake - a dropped network, a
// killed client, anything that produces a RST instead of a close frame - used
// to leave the read loop spinning forever. wsjson.Read failed instantly with
// net.ErrClosed on a connection coder/websocket had already closed, the loop
// wrote an error frame that also failed, and then read again, at full speed,
// for the life of the process. The handler never returned, so the connection's
// warm session was never disconnected and its retention pins were never
// released.
//
// goleak is the assertion because the goroutine was the symptom: nothing the
// client can observe distinguishes a handler that exited from one that is
// still spinning.
// Not parallel: goleak.IgnoreCurrent snapshots the goroutines alive when this
// test starts, so anything running alongside it would be reported as the leak
// it is looking for.
func TestWebSocketReadLoopEndsWhenTheClientVanishesWithoutAClose(t *testing.T) {
	ignore := goleak.IgnoreCurrent()
	gateway := &websocketStreamGateway{text: "ok"}
	hts := httptest.NewServer(New(config.Config{}, gateway, slog.Default()).Handler())
	defer hts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hts.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Drive one full response first, so the handler is parked in the read loop
	// with connection state to release rather than mid-handshake.
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "response.create", "model": "gpt-5", "input": "hi"}); err != nil {
		t.Fatal(err)
	}
	if resp := readUntilResponseCompleted(t, ctx, conn); resp == nil || resp.Status != "completed" {
		t.Fatalf("response = %#v, want completed", resp)
	}
	// CloseNow drops the TCP connection without sending a close frame, which is
	// what a client that dies rather than disconnects looks like on the wire.
	if err := conn.CloseNow(); err != nil {
		t.Fatal(err)
	}
	// Close returns without waiting for hijacked connections, so the handler
	// this test is about is still whatever it was: exited, or spinning.
	hts.Close()
	goleak.VerifyNone(t, ignore)
}

// blockedProducerGateway starts a producer that emits nothing and only returns
// when its context is cancelled. It is the shape of a real in-flight Copilot
// turn: the work is upstream, and cancellation is the only thing that ends it.
type blockedProducerGateway struct {
	unimplementedGateway
	started        chan struct{}
	producerExited chan struct{}
}

func (g *blockedProducerGateway) StreamResponse(ctx context.Context, _ copilotgw.ResponseRequest) (<-chan copilotgw.ResponseStreamEvent, error) {
	ch := make(chan copilotgw.ResponseStreamEvent)
	go func() {
		defer close(g.producerExited)
		defer close(ch)
		close(g.started)
		<-ctx.Done()
	}()
	return ch, nil
}

// A peer that stops reading is the case the close handshake is worst at:
// coder/websocket writes the close frame and then waits up to 5s for a reply
// that never arrives, behind the read mutex this connection's own read loop
// holds. Cancellation must not be queued behind that — the in-flight turn is
// burning upstream Copilot quota the whole time.
func TestWebSocketCloseCancelsWorkWithoutWaitingForTheCloseHandshake(t *testing.T) {
	t.Parallel()
	gateway := &blockedProducerGateway{started: make(chan struct{}), producerExited: make(chan struct{})}
	server := New(config.Config{}, gateway, slog.Default())
	hts := httptest.NewServer(server.Handler())
	defer hts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hts.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "response.create", "model": "gpt-5", "input": "hi"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gateway.started:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway producer never started")
	}
	// From here the client never reads again, so it will never answer the
	// server's close frame.
	go func() {
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelShutdown()
		_ = server.Shutdown(shutdownCtx)
	}()
	select {
	case <-gateway.producerExited:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight gateway work still running 2s after shutdown; cancellation is gated behind the close handshake")
	}
}

// The graceful close frame is part of the wire contract and must survive the
// reordering above: a client that is reading has to see StatusGoingAway, not a
// severed TCP connection.
func TestWebSocketShutdownStillCompletesTheCloseHandshake(t *testing.T) {
	t.Parallel()
	server := New(config.Config{}, &websocketStreamGateway{}, slog.Default())
	hts := httptest.NewServer(server.Handler())
	defer hts.Close()
	dialCtx, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDial()
	conn, _, err := websocket.Dial(dialCtx, "ws"+strings.TrimPrefix(hts.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	readResult := make(chan error, 1)
	go func() {
		readCtx, cancelRead := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelRead()
		var value any
		readResult <- wsjson.Read(readCtx, conn, &value)
	}()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	err = <-readResult
	if status := websocket.CloseStatus(err); status != websocket.StatusGoingAway {
		t.Fatalf("close status = %v (err = %v), want %v", status, err, websocket.StatusGoingAway)
	}
}

// The writer must not outlive the connection it writes to. A frame written to a
// peer whose socket is black-holed otherwise holds the writer mutex for the
// full 30s write timeout even though the connection died seconds earlier, which
// blocks every other frame on the connection behind it. The connection's write
// context - cancelled once conn.Close has returned, not when the connection's
// work is cancelled - is what bounds that.
func TestWebSocketWriterStopsWhenTheConnectionIsGone(t *testing.T) {
	t.Parallel()
	writeCtx, cancelWrites := context.WithCancel(context.Background())
	writer := &webSocketJSONWriter{ctx: writeCtx}
	ctx, cancel := writer.writeContext()
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > webSocketWriteTimeout {
		t.Fatalf("write deadline = %v (ok=%t), want at most %s out", deadline, ok, webSocketWriteTimeout)
	}
	cancelWrites()
	select {
	case <-ctx.Done():
	default:
		t.Fatal("write context outlived the connection; a write to a dead peer holds the writer mutex for the full write timeout")
	}
}
