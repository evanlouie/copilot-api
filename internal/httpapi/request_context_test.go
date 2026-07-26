package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

// requestContext's cancel func is the only thing a handler can use to stop the
// work it started. On SSE a no-op cancel is masked by net/http cancelling the
// request context when the handler returns, but a WebSocket handler's parent is
// the connection context, which outlives every individual response.
func TestRequestContextCancelStopsTheContextWithoutATimeout(t *testing.T) {
	t.Parallel()
	for _, timeout := range []time.Duration{0, -time.Second, time.Minute} {
		ctx, cancel := requestContext(context.Background(), timeout)
		cancel()
		select {
		case <-ctx.Done():
		default:
			t.Fatalf("requestContext(parent, %s) returned a context cancel() cannot stop", timeout)
		}
	}
}

// leakingStreamGateway emits one terminal error and then keeps trying to push
// deltas forever. A consumer that stops reading has to cancel the context to
// release the producer; nothing else will.
type leakingStreamGateway struct {
	unimplementedGateway
	producerExited chan struct{}
}

func (g *leakingStreamGateway) StreamResponse(ctx context.Context, _ copilotgw.ResponseRequest) (<-chan copilotgw.ResponseStreamEvent, error) {
	ch := make(chan copilotgw.ResponseStreamEvent)
	go func() {
		defer close(g.producerExited)
		defer close(ch)
		select {
		case ch <- copilotgw.ResponseStreamEvent{Kind: "error", Error: apierr.Upstream("upstream exploded")}:
		case <-ctx.Done():
			return
		}
		for {
			select {
			case ch <- copilotgw.ResponseStreamEvent{Kind: "delta", ItemID: "msg_final", Delta: "x"}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

// A WebSocket response that ends early (here: an upstream error) must release
// the gateway producer it started. Its parent is the connection context, so the
// only thing that can free the producer is the per-response cancel func.
func TestWebSocketResponseCancelsItsGatewayProducerOnEarlyReturn(t *testing.T) {
	t.Parallel()
	gateway := &leakingStreamGateway{producerExited: make(chan struct{})}
	hts := httptest.NewServer(New(config.Config{}, gateway, slog.Default()).Handler())
	defer hts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(hts.URL, "http")+"/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The connection stays open for the whole assertion: this is exactly the
	// state a real client is in between turns, and it is what makes a no-op
	// cancel a leak rather than a deferred cleanup.
	defer func() { _ = conn.CloseNow() }()
	if err := wsjson.Write(ctx, conn, map[string]any{"type": "response.create", "event_id": "evt", "model": "gpt-5", "input": "hi"}); err != nil {
		t.Fatal(err)
	}
	for {
		var event map[string]any
		if err := wsjson.Read(ctx, conn, &event); err != nil {
			t.Fatal(err)
		}
		eventType, _ := event["type"].(string)
		if eventType == "error" || eventType == "response.failed" {
			break
		}
		if eventType == "response.completed" {
			t.Fatalf("stream completed; the gateway was supposed to fail it: %#v", event)
		}
	}
	select {
	case <-gateway.producerExited:
	case <-time.After(5 * time.Second):
		t.Fatal("gateway producer goroutine still running after the response ended; the per-response cancel func never fired")
	}
}

// deadlineCaptureGateway records whether the context each read handler passed it
// carried the configured request deadline.
type deadlineCaptureGateway struct {
	unimplementedGateway
	resp        *openai.Response
	getDeadline bool
	delDeadline bool
}

func (g *deadlineCaptureGateway) GetResponse(ctx context.Context, id string) (*openai.Response, error) {
	_, g.getDeadline = ctx.Deadline()
	if g.resp == nil || g.resp.ID != id {
		return nil, apierr.NotFound("response not found", "not_found")
	}
	return g.resp, nil
}

func (g *deadlineCaptureGateway) DeleteResponse(ctx context.Context, id string) error {
	_, g.delDeadline = ctx.Deadline()
	if g.resp == nil || g.resp.ID != id {
		return apierr.NotFound("response not found", "not_found")
	}
	return nil
}

// Every other handler bounds its gateway work with COPILOT_REQUEST_TIMEOUT. The
// stored-response reads were the two that passed the raw request context
// straight through, so a wedged store read had no bound at all.
func TestStoredResponseHandlersApplyTheRequestTimeout(t *testing.T) {
	t.Parallel()
	gateway := &deadlineCaptureGateway{resp: &openai.Response{ID: "resp_1", Object: openai.ObjectResponse, Status: "completed"}}
	s := New(config.Config{RequestTimeout: time.Minute}, gateway, slog.Default())

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/responses/resp_1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d: %s", rec.Code, rec.Body.String())
	}
	if !gateway.getDeadline {
		t.Fatal("getResponse passed the gateway a context with no deadline despite RequestTimeout")
	}

	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/responses/resp_1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d: %s", rec.Code, rec.Body.String())
	}
	if !gateway.delDeadline {
		t.Fatal("deleteResponse passed the gateway a context with no deadline despite RequestTimeout")
	}
}

// idCaptureGateway records whatever response id the transport handed it.
type idCaptureGateway struct {
	unimplementedGateway
	sawID string
}

func (g *idCaptureGateway) GetResponse(_ context.Context, id string) (*openai.Response, error) {
	g.sawID = id
	return nil, apierr.NotFound("response not found", "not_found")
}

func (g *idCaptureGateway) DeleteResponse(_ context.Context, id string) error {
	g.sawID = id
	return apierr.NotFound("response not found", "not_found")
}

// The stored-response handlers used to slice the id out of r.URL.Path, which is
// the *decoded* path, while ServeMux routes on EscapedPath. That let a
// percent-encoded traversal segment through to the gateway, where only
// sessionstore.safeName stopped it — an invariant the transport does not own.
func TestStoredResponseHandlersRejectIDsTheProxyCannotHaveMinted(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"/v1/responses/%2E%2E",
		"/v1/responses/resp_%2E%2E",
		"/v1/responses/resp_%2E%2E%2E",
		"/v1/responses/notaresponse",
		"/v1/responses/resp_has%20space",
	} {
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			gateway := &idCaptureGateway{}
			rec := httptest.NewRecorder()
			New(config.Config{}, gateway, slog.Default()).Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("%s %s status = %d, want 400: %s", method, path, rec.Code, rec.Body.String())
			}
			if gateway.sawID != "" {
				t.Fatalf("%s %s reached the gateway as id %q", method, path, gateway.sawID)
			}
		}
	}
}
