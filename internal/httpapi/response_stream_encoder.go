package httpapi

import "github.com/evanlouie/copilot-api/internal/openai"

type responseStreamEncoder struct {
	writer         responseEventWriter
	sequenceNumber int64
}

func newResponseStreamEncoder(writer responseEventWriter) *responseStreamEncoder {
	return &responseStreamEncoder{writer: writer}
}

func (e *responseStreamEncoder) WriteResponseEvent(ev openai.ResponseStreamEvent) error {
	ev.SequenceNumber = e.sequenceNumber
	e.sequenceNumber++
	return e.writer.WriteResponseEvent(ev)
}
