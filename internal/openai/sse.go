package openai

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type SSEWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func NewSSEWriter(w http.ResponseWriter) (*SSEWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	return &SSEWriter{w: w, flusher: flusher}, true
}

func (s *SSEWriter) Data(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.DataJSON(b)
}

func (s *SSEWriter) DataJSON(b []byte) error {
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func (s *SSEWriter) Done() error {
	if _, err := fmt.Fprint(s.w, "data: [DONE]\n\n"); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func (s *SSEWriter) Event(event string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.EventJSON(event, b)
}

func (s *SSEWriter) EventJSON(event string, b []byte) error {
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}
