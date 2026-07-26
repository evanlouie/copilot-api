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
)

// A text frame that is not JSON at all is closed by wsjson.Read itself, with
// StatusInvalidFramePayloadData, before the error ever reaches this package
// (wsjson.go calls c.Close then returns the unmarshal error). 1007 is what the
// WebSocket spec prescribes for non-conforming payload data, and it is the
// behaviour this proxy wants.
//
// The read loop used to follow that with writer.writeError, which could only
// ever fail with net.ErrClosed - a dead write behind a comment claiming to
// "report what can still be reported". This pins what the client actually
// gets: the 1007 close and nothing else.
func TestWebSocketMalformedJSONClosesWith1007AndNoErrorEvent(t *testing.T) {
	t.Parallel()
	hts := httptest.NewServer(New(config.Config{}, &websocketStreamGateway{}, slog.Default()).Handler())
	defer hts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hts.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()

	if err := conn.Write(ctx, websocket.MessageText, []byte("this is not json")); err != nil {
		t.Fatal(err)
	}
	var raw any
	err = wsjson.Read(ctx, conn, &raw)
	if status := websocket.CloseStatus(err); status != websocket.StatusInvalidFramePayloadData {
		t.Fatalf("close status = %v (err = %v, frame = %#v), want %v with no frame in between", status, err, raw, websocket.StatusInvalidFramePayloadData)
	}
}

// The neighbouring path is live and must stay live: a frame that is valid JSON
// but not a JSON object never reaches wsjson's close, so the connection is
// still writable and the client gets a correlatable error envelope.
func TestWebSocketNonObjectJSONGetsAnErrorEventAndStaysOpen(t *testing.T) {
	t.Parallel()
	hts := httptest.NewServer(New(config.Config{}, &websocketStreamGateway{text: "ok"}, slog.Default()).Handler())
	defer hts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hts.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	if err := wsjson.Write(ctx, conn, "not an object"); err != nil {
		t.Fatal(err)
	}
	ev := readWebSocketErrorEvent(t, ctx, conn)
	if ev.Type != "error" || !strings.Contains(ev.Error.Message, "invalid JSON websocket message") {
		t.Fatalf("error event = %#v, want an invalid JSON websocket message error", ev)
	}
	// Still usable: the connection was never closed by the library.
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "response.create", "model": "gpt-5", "input": "hi"}); err != nil {
		t.Fatal(err)
	}
	if resp := readUntilResponseCompleted(t, ctx, conn); resp == nil || resp.Status != "completed" {
		t.Fatalf("response = %#v, want completed", resp)
	}
}
