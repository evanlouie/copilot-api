package httpapi

import (
	"context"
	"encoding/json"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"
)

// responseEventWriter serializes one Responses stream event. Every transport -
// SSE, WebSocket and the non-streaming JSON body - consumes the same event
// sequence through this interface, so a turn is described exactly once and each
// transport only decides how to put those events on the wire.
type responseEventWriter interface {
	WriteResponseEvent(openai.ResponseStreamEvent) error
}

// responseEventTransport puts an already-encoded Responses event on the wire.
// Encoding above the transport is what lets every transport share one debug
// logging path instead of each growing (or missing) its own.
type responseEventTransport interface {
	name() string
	writeResponseEventPayload(ev openai.ResponseStreamEvent, payload []byte) error
}

// loggedResponseEventWriter encodes each event once, hands the bytes to a
// transport, and emits the shared Responses debug log line. It is the only way
// a transport is wired up, so SSE and WebSocket are equally observable.
type loggedResponseEventWriter struct {
	server    *Server
	ctx       context.Context
	transport responseEventTransport
}

func newLoggedResponseEventWriter(s *Server, ctx context.Context, transport responseEventTransport) *loggedResponseEventWriter {
	return &loggedResponseEventWriter{server: s, ctx: ctx, transport: transport}
}

func (w *loggedResponseEventWriter) WriteResponseEvent(ev openai.ResponseStreamEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	start := time.Now()
	err = w.transport.writeResponseEventPayload(ev, payload)
	w.server.debugResponseStreamEvent(w.ctx, w.transport.name(), ev, payload, start, err)
	return err
}

type sseResponseEventTransport struct{ writer *SSEWriter }

func (t sseResponseEventTransport) name() string { return "sse" }

func (t sseResponseEventTransport) writeResponseEventPayload(ev openai.ResponseStreamEvent, payload []byte) error {
	return t.writer.EventJSON(ev.Type, payload)
}

// foldingResponseEventWriter reduces a Responses event sequence to its terminal
// response. It is the non-streaming transport: instead of marshaling a gateway
// result directly (and so bypassing everything the streaming transports apply),
// the JSON body is the response carried by the terminal event of the very same
// sequence SSE and WebSocket serialize.
type foldingResponseEventWriter struct {
	response *openai.Response
}

// isTerminalResponseEventType reports whether an event ends a response's stream.
// Every transport needs the same answer: the non-streaming body folds to the
// response such an event carries, and the WebSocket frees its response slot as
// it writes one.
func isTerminalResponseEventType(eventType string) bool {
	switch eventType {
	case "response.completed", "response.failed", "response.incomplete":
		return true
	}
	return false
}

func (w *foldingResponseEventWriter) WriteResponseEvent(ev openai.ResponseStreamEvent) error {
	if isTerminalResponseEventType(ev.Type) {
		w.response = ev.Response
	}
	return nil
}
