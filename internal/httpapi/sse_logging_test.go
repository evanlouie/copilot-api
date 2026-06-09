package httpapi

import (
	"bytes"
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/openai"
)

// TestWriteSSEDataContentGating asserts SSE-write debug logs include payload
// sizes and per-delta rune/byte counts, but only include the raw payload preview
// (which contains the streamed content) when cfg.LogContent is enabled.
func TestWriteSSEDataContentGating(t *testing.T) {
	const secret = "SUPER_SECRET_DELTA_TOKENS"
	cases := []struct {
		name        string
		logContent  bool
		wantPreview bool
	}{
		{"redacted", false, false},
		{"preview", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			s := &Server{cfg: config.Config{LogContent: tc.logContent}, log: logger}
			writer, ok := openai.NewSSEWriter(httptest.NewRecorder())
			if !ok {
				t.Fatal("NewSSEWriter returned not ok")
			}
			ctx := context.Background()
			chunk := openai.ChatCompletionChunk{ID: "chatcmpl_x", Object: openai.ObjectChatChunk, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Content: secret}}}}

			if err := s.writeSSEData(ctx, writer, "chat.content_delta", chunk, s.chatChunkAttrs(ctx, "content", secret)...); err != nil {
				t.Fatalf("writeSSEData: %v", err)
			}

			logs := buf.String()
			if !strings.Contains(logs, "payload_bytes") {
				t.Errorf("expected payload_bytes in log: %s", logs)
			}
			if !strings.Contains(logs, "delta_runes") {
				t.Errorf("expected delta_runes attr in log: %s", logs)
			}
			if tc.wantPreview {
				if !strings.Contains(logs, "payload_preview") || !strings.Contains(logs, secret) {
					t.Errorf("expected payload_preview with content when LogContent=true: %s", logs)
				}
			} else {
				if strings.Contains(logs, "payload_preview") {
					t.Errorf("unexpected payload_preview when LogContent=false: %s", logs)
				}
				if strings.Contains(logs, secret) {
					t.Errorf("content leaked into logs when LogContent=false: %s", logs)
				}
			}
		})
	}
}

// TestWriteSSEDataSkipsLoggingWhenDebugOff confirms the lazy debug guard short
// circuits at info level (no log work, no attr building) while still performing
// the actual SSE write.
func TestWriteSSEDataSkipsLoggingWhenDebugOff(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	s := &Server{cfg: config.Config{}, log: logger}
	rec := httptest.NewRecorder()
	writer, ok := openai.NewSSEWriter(rec)
	if !ok {
		t.Fatal("NewSSEWriter returned not ok")
	}
	ctx := context.Background()
	chunk := openai.ChatCompletionChunk{ID: "chatcmpl_x", Object: openai.ObjectChatChunk, Choices: []openai.ChatChunkChoice{{Index: 0, Delta: openai.ChatChunkDelta{Content: "hello world"}}}}

	if err := s.writeSSEData(ctx, writer, "chat.content_delta", chunk, s.chatChunkAttrs(ctx, "content", "hello world")...); err != nil {
		t.Fatalf("writeSSEData: %v", err)
	}

	if buf.Len() != 0 {
		t.Errorf("expected no debug logs at info level, got: %s", buf.String())
	}
	if !strings.Contains(rec.Body.String(), "hello world") {
		t.Errorf("expected SSE body to carry payload, got: %s", rec.Body.String())
	}
}
