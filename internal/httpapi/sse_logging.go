package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/observability"
	"github.com/evanlouie/copilot-api/internal/openai"
)

// debugEnabled reports whether debug-level stream logging is active. Hot-path
// callers use it to skip building log attributes (rune counts, payload marshals)
// when nothing will be emitted.
func (s *Server) debugEnabled(ctx context.Context) bool {
	return s != nil && s.log != nil && s.log.Enabled(ctx, slog.LevelDebug)
}

func (s *Server) debugStream(ctx context.Context, msg string, attrs ...any) {
	if !s.debugEnabled(ctx) {
		return
	}
	observability.Logger(ctx, s.log).Debug(msg, attrs...)
}

// chatChunkAttrs builds the debug attrs for a streamed chat chunk, computing the
// per-delta byte/rune sizes only when debug logging is enabled so the rune count
// is not paid per token in the common (info-level) case.
func (s *Server) chatChunkAttrs(ctx context.Context, chunkKind, delta string) []any {
	attrs := []any{"stream_kind", "chat", "chunk_kind", chunkKind}
	if delta != "" && s.debugEnabled(ctx) {
		attrs = append(attrs, streamDeltaAttrs(delta)...)
	}
	return attrs
}

func (s *Server) writeSSEData(ctx context.Context, writer *openai.SSEWriter, label string, v any, attrs ...any) error {
	if !s.debugEnabled(ctx) {
		return writer.Data(v)
	}
	start := time.Now()
	err := writer.Data(v)
	s.debugSSEWrite(ctx, "sse data written", label, start, err, v, attrs...)
	return err
}

func (s *Server) writeSSEDone(ctx context.Context, writer *openai.SSEWriter, attrs ...any) error {
	if !s.debugEnabled(ctx) {
		return writer.Done()
	}
	start := time.Now()
	err := writer.Done()
	s.debugSSEWrite(ctx, "sse done written", "done", start, err, nil, attrs...)
	return err
}

func (s *Server) writeSSEEvent(ctx context.Context, writer *openai.SSEWriter, event string, v any, attrs ...any) error {
	if !s.debugEnabled(ctx) {
		return writer.Event(event, v)
	}
	start := time.Now()
	err := writer.Event(event, v)
	attrs = append([]any{"event", event}, attrs...)
	s.debugSSEWrite(ctx, "sse event written", event, start, err, v, attrs...)
	return err
}

func (s *Server) debugSSEWrite(ctx context.Context, msg, label string, start time.Time, err error, v any, attrs ...any) {
	if !s.debugEnabled(ctx) {
		return
	}
	base := []any{"stream_label", label, "write_duration_ms", float64(time.Since(start).Microseconds()) / 1000.0}
	if err != nil {
		base = append(base, "error", err.Error())
	}
	if v != nil {
		if b, marshalErr := json.Marshal(v); marshalErr == nil {
			base = append(base, "payload_bytes", len(b))
			if s.cfg.LogContent {
				base = append(base, "payload_preview", observability.TruncateForLog(string(b), 240))
			}
		} else {
			base = append(base, "payload_marshal_error", marshalErr.Error())
		}
	}
	base = append(base, attrs...)
	observability.Logger(ctx, s.log).Debug(msg, base...)
}

func (s *Server) logChatStreamEvent(ctx context.Context, ev copilotgw.StreamEvent) {
	if !s.debugEnabled(ctx) {
		return
	}
	attrs := []any{"stream_kind", "chat", "event_kind", ev.Kind}
	if ev.Delta != "" {
		attrs = append(attrs, streamDeltaAttrs(ev.Delta)...)
	}
	if ev.Result != nil {
		attrs = append(attrs,
			"finish_reason", ev.Result.FinishReason,
			"result_text_bytes", len(ev.Result.Text),
			"result_text_runes", len([]rune(ev.Result.Text)),
			"result_reasoning_bytes", len(ev.Result.Reasoning),
			"tool_call_count", len(ev.Result.ToolCalls),
		)
	}
	if ev.Error != nil {
		attrs = append(attrs, "error", ev.Error.Error())
	}
	s.debugStream(ctx, "chat stream event received", attrs...)
}

func streamDeltaAttrs(delta string) []any {
	return []any{"delta_bytes", len(delta), "delta_runes", len([]rune(delta))}
}

func responseStreamEventAttrs(ev openai.ResponseStreamEvent) []any {
	attrs := []any{"stream_kind", "responses", "event_type", ev.Type, "sequence_number", ev.SequenceNumber}
	if ev.Delta != "" {
		attrs = append(attrs, streamDeltaAttrs(ev.Delta)...)
	}
	if ev.ItemID != "" {
		attrs = append(attrs, "item_id", ev.ItemID)
	}
	if ev.OutputIndex != nil {
		attrs = append(attrs, "output_index", *ev.OutputIndex)
	}
	if ev.ContentIndex != nil {
		attrs = append(attrs, "content_index", *ev.ContentIndex)
	}
	if ev.SummaryIndex != nil {
		attrs = append(attrs, "summary_index", *ev.SummaryIndex)
	}
	if ev.Status != "" {
		attrs = append(attrs, "status", ev.Status)
	}
	if ev.Response != nil {
		attrs = append(attrs, "response_id", ev.Response.ID, "response_status", ev.Response.Status, "output_text_bytes", len(ev.Response.OutputText), "output_item_count", len(ev.Response.Output))
	}
	if ev.Item != nil {
		attrs = append(attrs, "item_type", ev.Item.Type, "item_status", ev.Item.Status)
	}
	return attrs
}
