package httpapi

import "github.com/evanlouie/copilot-api/internal/openai"

type responseStreamEncoder struct {
	writer         responseEventWriter
	sequenceNumber int64
}

// newResponseStreamEncoder wraps writer so each event carries a monotonic
// sequence number. An encoder is returned unchanged so a caller that owns one
// (to keep numbering continuous across a later failure frame) can hand it to
// helpers that also normalise their writer.
func newResponseStreamEncoder(writer responseEventWriter) *responseStreamEncoder {
	if encoder, ok := writer.(*responseStreamEncoder); ok {
		return encoder
	}
	return &responseStreamEncoder{writer: writer}
}

func (e *responseStreamEncoder) WriteResponseEvent(ev openai.ResponseStreamEvent) error {
	ev.SequenceNumber = e.nextSequence()
	return e.writer.WriteResponseEvent(ev)
}

// nextSequence reserves this response's next sequence number for a frame the
// encoder does not serialize itself. The WebSocket error envelope is a distinct
// type from ResponseStreamEvent but belongs to the same response's event
// sequence, and OpenAI numbers every Responses stream event including `error`.
func (e *responseStreamEncoder) nextSequence() int64 {
	n := e.sequenceNumber
	e.sequenceNumber++
	return n
}
