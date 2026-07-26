package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/evanlouie/copilot-api/internal/apierr"
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
	if ev.Type != "error" || !strings.Contains(ev.Message, "invalid JSON websocket message") {
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

// The Responses WebSocket error frame is asserted on the wire, not through its
// Go type, because the defect this guards against is invisible to a Go decode:
// the frame was well formed and carried a correct message, but omitted
// sequence_number. Every schema an OpenAI client uses to recognise an `error`
// stream event requires that field, so its absence does not surface as a
// malformed frame - it makes the frame stop being an error frame at all.
//
// Measured against @ai-sdk/openai 4.0.20, whose chunk union is
// [nested-error, flat-error, {type: string}.loose()]: without sequence_number
// both error branches fail and the frame matches the catch-all, which the
// stream transform maps to "unknown_chunk" and drops. The AI SDK Deno suite
// observed exactly that - a client sending a bad previous_response_id saw a
// clean, empty, successful stream instead of a 400.
//
// The flat and nested spellings are both asserted because OpenAI's own clients
// disagree: openai-dotnet reads code/message/param at the top level per the
// published ResponseErrorEvent, while openai-python reads error.message.
func TestWebSocketErrorFrameCarriesTheFieldsClientsMatchOn(t *testing.T) {
	t.Parallel()
	gw := &errorResponseGateway{err: apierr.PreviousResponseNotFound("resp_missing")}
	conn, cleanup := newResponsesWebSocketConn(t, gw)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := wsjson.Write(ctx, conn, map[string]any{
		"type":                 "response.create",
		"event_id":             "evt_seq",
		"model":                "gpt-5",
		"previous_response_id": "resp_missing",
		"input":                "hi",
	}); err != nil {
		t.Fatal(err)
	}

	var raw json.RawMessage
	if err := wsjson.Read(ctx, conn, &raw); err != nil {
		t.Fatal(err)
	}
	var frame map[string]json.RawMessage
	if err := json.Unmarshal(raw, &frame); err != nil {
		t.Fatalf("error frame is not a JSON object: %v (%s)", err, raw)
	}

	if _, ok := frame["sequence_number"]; !ok {
		t.Fatalf("error frame has no sequence_number, so no OpenAI client can recognise it: %s", raw)
	}
	var seq json.Number
	if err := json.Unmarshal(frame["sequence_number"], &seq); err != nil {
		t.Fatalf("sequence_number is not a number: %v (%s)", err, raw)
	}
	if seq.String() != "0" {
		t.Fatalf("sequence_number = %s, want 0 for a response that emitted nothing before failing", seq)
	}

	var flat struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
		Param   string `json:"param"`
	}
	if err := json.Unmarshal(raw, &flat); err != nil {
		t.Fatal(err)
	}
	if flat.Type != "error" {
		t.Fatalf("type = %q, want error", flat.Type)
	}
	if flat.Code != "previous_response_not_found" || flat.Param != "previous_response_id" || flat.Message == "" {
		t.Fatalf("flat error fields = %#v, want the failure described at the top level", flat)
	}

	var nested struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
			Param   string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &nested); err != nil {
		t.Fatal(err)
	}
	if nested.Error.Code != flat.Code || nested.Error.Message != flat.Message || nested.Error.Param != flat.Param {
		t.Fatalf("nested error = %#v, want it to agree with the flat fields %#v", nested.Error, flat)
	}
	if nested.Error.Type != "invalid_request_error" {
		t.Fatalf("nested error type = %q, want invalid_request_error", nested.Error.Type)
	}
}
