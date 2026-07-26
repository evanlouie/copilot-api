package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
)

// deltaFloodGateway streams deltas as fast as the connection will take them and
// only stops when its context dies. It exists to keep the connection's writer
// inside coder/websocket's writeFrame for as much of the test as possible: the
// close-handshake race this file pins can only be observed while a data frame
// is genuinely in flight.
type deltaFloodGateway struct {
	unimplementedGateway
	startOnce sync.Once
	started   chan struct{}
}

func (g *deltaFloodGateway) StreamResponse(ctx context.Context, _ copilotgw.ResponseRequest) (<-chan copilotgw.ResponseStreamEvent, error) {
	ch := make(chan copilotgw.ResponseStreamEvent)
	go func() {
		defer close(ch)
		g.startOnce.Do(func() { close(g.started) })
		for {
			select {
			case <-ctx.Done():
				return
			case ch <- copilotgw.ResponseStreamEvent{Kind: "delta", ItemID: "msg_final", Delta: strings.Repeat("x", 512)}:
			}
		}
	}()
	return ch, nil
}

// Shutting a connection down while a response frame is mid-write must still
// produce a 1001 close, not a severed TCP connection.
//
// coder/websocket's writeFrame installs a context.AfterFunc on the write
// context that calls Conn.close (see setupWriteTimeout in conn.go). If the
// context bounding frame writes is the same context that shutdown cancels to
// release upstream work, cancelling it while a frame is in flight closes the
// socket outright, and the close frame that follows is dropped by
// writeFrame's `select { case <-c.closed: return net.ErrClosed }`. The client
// sees EOF (CloseStatus -1) instead of StatusGoingAway.
//
// The scenario is sampled repeatedly inside one test run because it is a race:
// a single pass proves nothing.
func TestWebSocketShutdownCompletesTheCloseHandshakeMidStream(t *testing.T) {
	t.Parallel()
	const samples = 8
	abnormal := 0
	for range samples {
		if status := runWebSocketMidStreamShutdown(t); status != websocket.StatusGoingAway {
			abnormal++
		}
	}
	if abnormal != 0 {
		t.Fatalf("%d/%d shutdowns closed abnormally instead of %v", abnormal, samples, websocket.StatusGoingAway)
	}
}

// runWebSocketMidStreamShutdown drives one response.create against a gateway
// that never stops streaming, shuts the server down once frames are actually
// flowing, and reports the close status the client observed.
func runWebSocketMidStreamShutdown(t *testing.T) websocket.StatusCode {
	t.Helper()
	gateway := &deltaFloodGateway{started: make(chan struct{})}
	server := New(config.Config{}, gateway, slog.New(slog.DiscardHandler))
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
	case <-time.After(10 * time.Second):
		t.Fatal("gateway producer never started")
	}

	// Drain continuously. A client that stops reading would stall the writer on
	// a full socket buffer, and then the close frame could not be written
	// either - that is a different failure, not the one under test.
	flowing := make(chan struct{})
	readErr := make(chan error, 1)
	go func() {
		var once sync.Once
		for {
			var raw json.RawMessage
			if err := wsjson.Read(ctx, conn, &raw); err != nil {
				readErr <- err
				return
			}
			once.Do(func() { close(flowing) })
		}
	}()
	select {
	case <-flowing:
	case err := <-readErr:
		t.Fatalf("read failed before any frame arrived: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("no frame ever reached the client")
	}
	// Let the writer settle into its steady state so shutdown lands on a write
	// rather than on the handler still setting the stream up.
	time.Sleep(20 * time.Millisecond)

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case err := <-readErr:
		return websocket.CloseStatus(err)
	case <-time.After(15 * time.Second):
		t.Fatal("client never observed the connection closing")
	}
	return 0
}
