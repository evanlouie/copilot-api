package observability

import (
	"context"
	"log/slog"
	"net/http"
	"unicode/utf8"

	"github.com/google/uuid"
)

type requestIDKey struct{}

func NewRequestID() string { return "req_" + uuid.NewString() }

func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

func Logger(ctx context.Context, base *slog.Logger) *slog.Logger {
	if id := RequestID(ctx); id != "" {
		return base.With("request_id", id)
	}
	return base
}

func RedactedContent(enabled bool, content string) string {
	if enabled {
		return content
	}
	if content == "" {
		return ""
	}
	return "[redacted]"
}

// TruncateForLog returns s limited to maxRunes runes, appending an ellipsis when
// truncated. A non-positive maxRunes yields an empty string. It is shared by the
// debug-logging paths so content previews truncate identically everywhere.
func TruncateForLog(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	count := 0
	for byteIndex := range s {
		if count == maxRunes {
			return s[:byteIndex] + "\u2026"
		}
		count++
	}
	return s
}

func TruncateBytesForLog(b []byte, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	byteIndex := 0
	for count := 0; byteIndex < len(b); count++ {
		if count == maxRunes {
			return string(b[:byteIndex]) + "\u2026"
		}
		_, size := utf8.DecodeRune(b[byteIndex:])
		byteIndex += size
	}
	return string(b)
}

func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = NewRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}
