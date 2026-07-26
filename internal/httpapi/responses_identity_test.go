package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

// identityGateway models the real gateway's contract: it builds the turn's
// openai.Response exactly once, assigns every output-item ID itself, persists
// that object, and streams the very same object as the terminal event. Any ID
// or ordering the HTTP layer invents for itself therefore shows up as a
// divergence between the streamed events, the terminal response and a later
// GET.
type identityGateway struct {
	copilotgw.Gateway
	mu     sync.Mutex
	stored map[string]*openai.Response
}

const (
	identityReasoningItemID = "rs_gateway_assigned"
	identityMessageItemID   = "msg_gateway_assigned"
)

func (g *identityGateway) buildAndPersist(req copilotgw.ResponseRequest) *openai.Response {
	resp := &openai.Response{
		ID:         req.ResponseID,
		Object:     openai.ObjectResponse,
		CreatedAt:  openai.UnixNow(),
		Status:     "completed",
		Model:      req.Model,
		OutputText: "answer",
		Output: []openai.ResponseOutputItem{
			{ID: identityReasoningItemID, Type: "reasoning", Status: "completed", Summary: []openai.ResponseReasoningSummary{{Type: "summary_text", Text: "thinking"}}},
			{ID: identityMessageItemID, Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: "answer"}}},
		},
		ParallelToolCalls: true,
		Store:             req.Store,
	}
	// Persist a snapshot, exactly as sessionstore does: the record is written to
	// disk at this point, so later in-place edits by the HTTP layer cannot reach
	// it.
	g.mu.Lock()
	if g.stored == nil {
		g.stored = map[string]*openai.Response{}
	}
	g.stored[resp.ID] = snapshotResponse(resp)
	g.mu.Unlock()
	return resp
}

func (g *identityGateway) StreamResponse(_ context.Context, req copilotgw.ResponseRequest) (<-chan copilotgw.ResponseStreamEvent, error) {
	resp := g.buildAndPersist(req)
	ch := make(chan copilotgw.ResponseStreamEvent, 3)
	go func() {
		defer close(ch)
		ch <- copilotgw.ResponseStreamEvent{Kind: "reasoning_delta", ItemID: identityReasoningItemID, Delta: "thinking"}
		ch <- copilotgw.ResponseStreamEvent{Kind: "delta", ItemID: identityMessageItemID, Delta: "answer"}
		ch <- copilotgw.ResponseStreamEvent{Kind: "response", Response: resp}
	}()
	return ch, nil
}

func (g *identityGateway) CreateResponse(_ context.Context, req copilotgw.ResponseRequest) (*copilotgw.ResponseResult, error) {
	return &copilotgw.ResponseResult{Response: g.buildAndPersist(req)}, nil
}

func snapshotResponse(resp *openai.Response) *openai.Response {
	encoded, err := json.Marshal(resp)
	if err != nil {
		panic(err)
	}
	var out openai.Response
	if err := json.Unmarshal(encoded, &out); err != nil {
		panic(err)
	}
	return &out
}

func (g *identityGateway) GetResponse(_ context.Context, id string) (*openai.Response, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	resp, ok := g.stored[id]
	if !ok {
		return nil, apierr.NotFound("response not found", "not_found")
	}
	return resp, nil
}

func streamResponseOverSSE(t *testing.T, s *Server) []openai.ResponseStreamEvent {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","stream":true,"input":"hi"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	return parseResponseStreamEvents(t, rec.Body.String())
}

func streamResponseOverWebSocket(t *testing.T, s *Server) []openai.ResponseStreamEvent {
	t.Helper()
	hts := httptest.NewServer(s.Handler())
	defer hts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hts.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "response.create", "model": "gpt-5", "input": "hi"}); err != nil {
		t.Fatal(err)
	}
	var events []openai.ResponseStreamEvent
	for {
		var raw json.RawMessage
		if err := wsjson.Read(ctx, conn, &raw); err != nil {
			t.Fatal(err)
		}
		// Decode strictly into the stream event type: comparing transports means
		// comparing every field the wire carried, including `status`.
		var ev openai.ResponseStreamEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			t.Fatalf("failed to unmarshal websocket event %s: %v", raw, err)
		}
		events = append(events, ev)
		if ev.Type == "response.completed" || ev.Type == "response.failed" || ev.Type == "error" {
			return events
		}
	}
}

// itemIDsIn collects every output-item ID the stream named, both as item_id on
// part/delta events and as the id of an announced item.
func itemIDsIn(events []openai.ResponseStreamEvent) map[string]struct{} {
	ids := map[string]struct{}{}
	for _, ev := range events {
		if ev.ItemID != "" {
			ids[ev.ItemID] = struct{}{}
		}
		if ev.Item != nil && ev.Item.ID != "" {
			ids[ev.Item.ID] = struct{}{}
		}
	}
	return ids
}

func terminalResponse(t *testing.T, events []openai.ResponseStreamEvent) *openai.Response {
	t.Helper()
	for _, ev := range events {
		if ev.Type == "response.completed" {
			if ev.Response == nil {
				t.Fatal("response.completed carried no response")
			}
			return ev.Response
		}
	}
	t.Fatalf("no response.completed in %v", eventTypes(events))
	return nil
}

func fetchStoredResponse(t *testing.T, s *Server, id string) *openai.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/responses/"+id, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/responses/%s = %d: %s", id, rec.Code, rec.Body.String())
	}
	var stored openai.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &stored); err != nil {
		t.Fatalf("decode GET body %s: %v", rec.Body.String(), err)
	}
	return &stored
}

// TestStreamedPersistedAndFetchedOutputItemIDsAgree covers invariant 1: the
// output-item IDs in the streamed events, in the terminal response and in a
// later GET are the same, and each item's streamed output_index is its position
// in the stored output.
func TestStreamedPersistedAndFetchedOutputItemIDsAgree(t *testing.T) {
	gw := &identityGateway{}
	s := New(config.Config{}, gw, slog.Default())

	events := streamResponseOverSSE(t, s)
	completed := terminalResponse(t, events)
	stored := fetchStoredResponse(t, s, completed.ID)

	if len(stored.Output) != 2 {
		t.Fatalf("stored output = %#v", stored.Output)
	}
	streamedIDs := itemIDsIn(events)
	for i, item := range stored.Output {
		if completed.Output[i].ID != item.ID {
			t.Errorf("output[%d]: response.completed id = %q, GET /v1/responses/%s id = %q", i, completed.Output[i].ID, completed.ID, item.ID)
		}
		if _, ok := streamedIDs[item.ID]; !ok {
			t.Errorf("persisted output item id %q never appeared in the streamed events; streamed ids = %v", item.ID, streamedIDs)
		}
	}
	// The streamed output_index of an item must equal its position in the stored
	// output, or a client correlating streamed items against the stored record
	// attaches content to the wrong item.
	for _, ev := range events {
		if ev.Item == nil || ev.OutputIndex == nil {
			continue
		}
		idx := *ev.OutputIndex
		if idx >= len(stored.Output) || stored.Output[idx].ID != ev.Item.ID {
			t.Errorf("%s announced item %q at output_index %d, stored output = %v", ev.Type, ev.Item.ID, idx, storedItemIDs(stored))
		}
	}
}

func storedItemIDs(resp *openai.Response) []string {
	ids := make([]string, 0, len(resp.Output))
	for _, item := range resp.Output {
		ids = append(ids, item.ID)
	}
	return ids
}

// TestTerminalCompletedPayloadMatchesPersistedRecord covers invariant 2: the
// response.completed payload and the persisted record are the same object, so
// they serialize identically.
func TestTerminalCompletedPayloadMatchesPersistedRecord(t *testing.T) {
	gw := &identityGateway{}
	s := New(config.Config{}, gw, slog.Default())

	events := streamResponseOverSSE(t, s)
	completed := terminalResponse(t, events)
	stored := fetchStoredResponse(t, s, completed.ID)

	completedJSON, err := json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	storedJSON, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if string(completedJSON) != string(storedJSON) {
		t.Fatalf("terminal payload and stored record diverged:\n completed = %s\n stored    = %s", completedJSON, storedJSON)
	}
}

// TestNonStreamingBodyIsTheFoldedTerminalEvent covers the third transport: the
// non-streaming JSON body is the terminal event of the same event sequence the
// streaming transports serialize, not an independent rendering of the gateway
// result.
func TestNonStreamingBodyIsTheFoldedTerminalEvent(t *testing.T) {
	gw := &identityGateway{}
	s := New(config.Config{}, gw, slog.Default())

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5","input":"hi"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body openai.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	stored := fetchStoredResponse(t, s, body.ID)
	bodyJSON, err := json.Marshal(&body)
	if err != nil {
		t.Fatal(err)
	}
	storedJSON, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if string(bodyJSON) != string(storedJSON) {
		t.Fatalf("non-streaming body and stored record diverged:\n body   = %s\n stored = %s", bodyJSON, storedJSON)
	}
}

// TestResponseCreatedAndCompletedShareOneTimestamp pins the timestamp fix: the
// lifecycle frame at the start of a stream, the terminal response and the
// stored record all report the same created_at. The terminal response used to
// re-read the clock at emission time and disagree with response.created.
func TestResponseCreatedAndCompletedShareOneTimestamp(t *testing.T) {
	gw := &identityGateway{}
	s := New(config.Config{}, gw, slog.Default())

	events := streamResponseOverSSE(t, s)
	var created *openai.Response
	for _, ev := range events {
		if ev.Type == "response.created" {
			created = ev.Response
		}
	}
	if created == nil {
		t.Fatalf("no response.created in %v", eventTypes(events))
	}
	completed := terminalResponse(t, events)
	stored := fetchStoredResponse(t, s, completed.ID)
	if created.CreatedAt != completed.CreatedAt || completed.CreatedAt != stored.CreatedAt {
		t.Fatalf("created_at diverged: response.created=%d response.completed=%d stored=%d", created.CreatedAt, completed.CreatedAt, stored.CreatedAt)
	}
}

// responseEventShape is the transport-independent projection of an event: the
// per-request response id and creation time necessarily differ between two
// requests, but everything that describes the turn must not.
func responseEventShape(ev openai.ResponseStreamEvent) string {
	shape := fmt.Sprintf("seq=%d type=%s status=%s item_id=%s delta=%q text=%q", ev.SequenceNumber, ev.Type, ev.Status, ev.ItemID, ev.Delta, ev.Text)
	if ev.OutputIndex != nil {
		shape += fmt.Sprintf(" output_index=%d", *ev.OutputIndex)
	}
	if ev.ContentIndex != nil {
		shape += fmt.Sprintf(" content_index=%d", *ev.ContentIndex)
	}
	if ev.SummaryIndex != nil {
		shape += fmt.Sprintf(" summary_index=%d", *ev.SummaryIndex)
	}
	if ev.Item != nil {
		shape += fmt.Sprintf(" item=%s/%s/%s", ev.Item.ID, ev.Item.Type, ev.Item.Status)
	}
	if ev.Response != nil {
		shape += fmt.Sprintf(" response=%s/%v/%q", ev.Response.Status, storedItemIDs(ev.Response), ev.Response.OutputText)
	}
	return shape
}

func responseEventShapes(events []openai.ResponseStreamEvent) []string {
	shapes := make([]string, len(events))
	for i, ev := range events {
		shapes[i] = responseEventShape(ev)
	}
	return shapes
}

// TestSSEAndWebSocketEmitTheSameEventSequence covers invariant 3: both
// streaming transports serialize one shared event sequence, so they agree event
// for event, including the sequence_number progression.
func TestSSEAndWebSocketEmitTheSameEventSequence(t *testing.T) {
	sse := responseEventShapes(streamResponseOverSSE(t, New(config.Config{}, &identityGateway{}, slog.Default())))
	ws := responseEventShapes(streamResponseOverWebSocket(t, New(config.Config{}, &identityGateway{}, slog.Default())))
	if len(sse) != len(ws) {
		t.Fatalf("sse emitted %d events, websocket emitted %d:\n sse = %v\n ws  = %v", len(sse), len(ws), sse, ws)
	}
	for i := range sse {
		if sse[i] != ws[i] {
			t.Fatalf("event %d diverged:\n sse = %s\n ws  = %s", i, sse[i], ws[i])
		}
	}
}

// TestWebSocketResponseEventsAreLoggedLikeSSE covers invariant 4: the
// WebSocket transport is no longer a black box. Both transports emit the same
// debug record for the same event, distinguished only by the transport name.
func TestWebSocketResponseEventsAreLoggedLikeSSE(t *testing.T) {
	debugServer := func(sink *syncBuffer) *Server {
		logger := slog.New(slog.NewJSONHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))
		return New(config.Config{}, &identityGateway{}, logger)
	}
	var sseLogs, wsLogs syncBuffer
	streamResponseOverSSE(t, debugServer(&sseLogs))
	streamResponseOverWebSocket(t, debugServer(&wsLogs))

	for _, tc := range []struct {
		transport string
		logs      string
	}{{"sse", sseLogs.String()}, {"websocket", wsLogs.String()}} {
		for _, want := range []string{`"msg":"responses stream event written"`, `"transport":"` + tc.transport + `"`, `"event_type":"response.completed"`, `"event_type":"response.output_text.delta"`, `"sequence_number"`, `"payload_bytes"`} {
			if !strings.Contains(tc.logs, want) {
				t.Errorf("%s stream logs missing %s: %s", tc.transport, want, tc.logs)
			}
		}
	}
}

// TestResponseStreamRejectsDeltaWithoutGatewayItemID pins the contract that
// makes single ID assignment possible: the gateway names the output item a
// delta belongs to. The HTTP layer must not paper over a missing ID by minting
// one, because the gateway has already persisted the item under its own.
func TestResponseStreamRejectsDeltaWithoutGatewayItemID(t *testing.T) {
	channel := make(chan copilotgw.ResponseStreamEvent, 1)
	channel <- copilotgw.ResponseStreamEvent{Kind: "delta", Delta: "answer"}
	close(channel)
	writer := &captureResponseEventWriter{}
	result := writeResponseStreamEvents(context.Background(), writer, copilotgw.ResponseRequest{ResponseID: "resp_no_id", Model: "gpt-5"}, 0, false, channel)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "missing its output item id") {
		t.Fatalf("result = %#v, want a missing item id failure", result)
	}
	for _, ev := range writer.events {
		if ev.Item != nil && ev.Item.Type == "message" {
			t.Fatalf("a message item was announced under an invented id: %#v", ev.Item)
		}
	}
}

// TestResponseStreamRejectsRenamedTerminalMessageItem covers the other half of
// that contract: a terminal response whose message item does not carry the ID
// the stream already announced means two components are assigning IDs, which is
// reported rather than rewritten.
func TestResponseStreamRejectsRenamedTerminalMessageItem(t *testing.T) {
	channel := make(chan copilotgw.ResponseStreamEvent, 2)
	channel <- copilotgw.ResponseStreamEvent{Kind: "delta", ItemID: "msg_streamed", Delta: "answer"}
	channel <- copilotgw.ResponseStreamEvent{Kind: "response", Response: &openai.Response{
		ID: "resp_renamed", Object: openai.ObjectResponse, Status: "completed", Model: "gpt-5", OutputText: "answer",
		Output: []openai.ResponseOutputItem{{ID: "msg_other", Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: "answer"}}}},
	}}
	close(channel)
	writer := &captureResponseEventWriter{}
	result := writeResponseStreamEvents(context.Background(), writer, copilotgw.ResponseRequest{ResponseID: "resp_renamed", Model: "gpt-5"}, 0, false, channel)
	if result.Err == nil || !strings.Contains(result.Err.Error(), "does not match the streamed item") {
		t.Fatalf("result = %#v, want a terminal item id mismatch failure", result)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
