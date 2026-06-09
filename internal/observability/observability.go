package observability

import (
	"context"
	"log/slog"
	"net/http"

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
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "\u2026"
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
