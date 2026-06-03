package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/evanlouie/copilot-api/internal/observability"
	"github.com/evanlouie/copilot-api/internal/openai"
)

func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	body := r.Body
	if maxBytes > 0 {
		body = http.MaxBytesReader(w, r.Body, maxBytes)
	}
	defer r.Body.Close()
	dec := json.NewDecoder(body)
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		return openai.InvalidRequest("invalid JSON request body: "+err.Error(), "body")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return openai.InvalidRequest("request body must contain a single JSON object", "body")
	}
	return nil
}
func requestContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return parent, func() {}
	}
	return context.WithTimeout(parent, timeout)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") || s.cfg.APIKey == "" {
			next.ServeHTTP(w, r)
			return
		}
		if !validBearerToken(r.Header.Values("Authorization"), s.cfg.APIKey) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="copilot-api"`)
			openai.WriteError(w, openai.Unauthorized("invalid bearer token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
func validBearerToken(values []string, apiKey string) bool {
	if len(values) != 1 {
		return false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return false
	}
	supplied := sha256.Sum256([]byte(parts[1]))
	expected := sha256.Sum256([]byte(apiKey))
	return subtle.ConstantTimeCompare(supplied[:], expected[:]) == 1
}

type requestLogMetadata struct {
	mu              sync.Mutex
	model           string
	reasoningEffort string
}
type requestLogMetadataKey struct{}

func requestLoggingMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		meta := &requestLogMetadata{}
		r = r.WithContext(context.WithValue(r.Context(), requestLogMetadataKey{}, meta))
		recorder := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		duration := time.Since(start)
		logger := observability.Logger(r.Context(), log)
		attrs := []any{
			"method", r.Method,
			"path", r.URL.EscapedPath(),
			"model", meta.Model(),
			"status", recorder.status,
			"bytes", recorder.bytes,
			"duration_ms", float64(duration.Microseconds()) / 1000.0,
			"remote_ip", remoteIP(r.RemoteAddr),
		}
		if reasoningEffort := meta.ReasoningEffort(); reasoningEffort != "" {
			attrs = append(attrs, "reasoning_effort", reasoningEffort)
		}
		if ua := r.UserAgent(); ua != "" {
			attrs = append(attrs, "user_agent", ua)
		}
		switch {
		case recorder.status >= 500:
			logger.Error("request completed", attrs...)
		case recorder.status >= 400:
			logger.Warn("request completed", attrs...)
		default:
			logger.Info("request completed", attrs...)
		}
	})
}
func setRequestLogModel(r *http.Request, model string) {
	meta, ok := r.Context().Value(requestLogMetadataKey{}).(*requestLogMetadata)
	if !ok || meta == nil {
		return
	}
	meta.SetModel(model)
}
func setRequestLogReasoningEffort(r *http.Request, reasoningEffort string) {
	meta, ok := r.Context().Value(requestLogMetadataKey{}).(*requestLogMetadata)
	if !ok || meta == nil {
		return
	}
	meta.SetReasoningEffort(reasoningEffort)
}
func (m *requestLogMetadata) SetModel(model string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.model = model
}
func (m *requestLogMetadata) SetReasoningEffort(reasoningEffort string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reasoningEffort = reasoningEffort
}
func (m *requestLogMetadata) Model() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.model
}
func (m *requestLogMetadata) ReasoningEffort() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reasoningEffort
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.status = status
	w.wrote = true
	w.ResponseWriter.WriteHeader(status)
}
func (w *loggingResponseWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}
func (w *loggingResponseWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
func (w *loggingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
func remoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return remoteAddr
}
func recoverMiddleware(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				observability.Logger(r.Context(), log).Error("panic in HTTP handler", "panic", v, "stack", string(debug.Stack()))
				openai.WriteError(w, openai.Internal("internal server error"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}
func asAPIError(err error) *openai.APIError {
	var api *openai.APIError
	if errors.As(err, &api) {
		return api
	}
	return openai.Internal(err.Error())
}
func errorObject(err error) openai.ErrorObject {
	api := asAPIError(err)
	return openai.ErrorObject{Message: api.Message, Type: api.Type, Param: api.Param, Code: api.Code}
}
