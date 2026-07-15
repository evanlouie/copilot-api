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
	defer func() { _ = r.Body.Close() }()
	dec := json.NewDecoder(body)
	dec.UseNumber()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return openai.RequestTooLarge()
		}
		return openai.InvalidRequest("invalid JSON request body: "+err.Error(), "body")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return openai.RequestTooLarge()
		}
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
			if s.authFailures.Allow(remoteIP(r.RemoteAddr), time.Now()) && s.log != nil {
				attrs := []any{"path", r.URL.EscapedPath(), "remote_ip", remoteIP(r.RemoteAddr)}
				if ua := boundedUserAgent(r.UserAgent()); ua != "" {
					attrs = append(attrs, "user_agent", ua)
				}
				observability.Logger(r.Context(), s.log).Warn("authentication failed", attrs...)
			}
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
	mu                      sync.Mutex
	endpoint                string
	model                   string
	reasoningEffort         string
	resolvedReasoningEffort string
	continuation            bool
}

type requestLogFields struct {
	Endpoint                string
	Model                   string
	ReasoningEffort         string
	ResolvedReasoningEffort string
	Continuation            bool
}

type requestLogMetadataKey struct{}

type reasoningEffortResolver interface {
	ResolveReasoningEffort(ctx context.Context, model, requestedEffort, defaultEffort string) (string, error)
}

// maxLoggedBodyBytes caps how much of a request or response body is captured
// for content logging. Streaming responses (SSE) can be very large; we keep
// only the head to bound memory and log volume.
const maxLoggedBodyBytes = 64 << 10

func requestLoggingMiddleware(log *slog.Logger, logContent bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		meta := &requestLogMetadata{}
		r = r.WithContext(context.WithValue(r.Context(), requestLogMetadataKey{}, meta))

		var reqCapture *bodyCapture
		if logContent && r.Body != nil && r.Body != http.NoBody {
			reqCapture = newBodyCapture(maxLoggedBodyBytes)
			r.Body = &teeReadCloser{rc: r.Body, buf: reqCapture, captureActive: true}
		}

		logger := observability.Logger(r.Context(), log)
		startAttrs := []any{
			"method", r.Method,
			"path", r.URL.EscapedPath(),
			"remote_ip", remoteIP(r.RemoteAddr),
		}
		if ua := boundedUserAgent(r.UserAgent()); ua != "" {
			startAttrs = append(startAttrs, "user_agent", ua)
		}
		logger.Debug("request received", startAttrs...)

		recorder := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		if logContent {
			recorder.capture = newBodyCapture(maxLoggedBodyBytes)
			recorder.captureActive = true
		}
		next.ServeHTTP(recorder, r)

		duration := time.Since(start)
		fields := meta.Fields()
		attrs := []any{
			"method", r.Method,
			"path", r.URL.EscapedPath(),
			"status", recorder.status,
			"bytes", recorder.bytes,
			"duration_ms", float64(duration.Microseconds()) / 1000.0,
			"remote_ip", remoteIP(r.RemoteAddr),
		}
		if fields.Endpoint != "" {
			attrs = append(attrs, "endpoint", fields.Endpoint)
		}
		if fields.Model != "" {
			attrs = append(attrs, "model", fields.Model)
		}
		if fields.Endpoint != "" || fields.ReasoningEffort != "" {
			attrs = append(attrs, "reasoning_effort", fields.ReasoningEffort)
		}
		if fields.ResolvedReasoningEffort != "" && fields.ResolvedReasoningEffort != fields.ReasoningEffort {
			attrs = append(attrs, "reasoning_effort_resolved", fields.ResolvedReasoningEffort)
		}
		if fields.Continuation {
			attrs = append(attrs, "continuation", true)
		}
		if ua := boundedUserAgent(r.UserAgent()); ua != "" {
			attrs = append(attrs, "user_agent", ua)
		}
		if logContent {
			if reqCapture != nil {
				attrs = append(attrs, "request_body", reqCapture.String())
				if reqCapture.Truncated() {
					attrs = append(attrs, "request_body_truncated", true)
				}
			}
			if recorder.capture != nil {
				attrs = append(attrs, "response_body", recorder.capture.String())
				if recorder.capture.Truncated() {
					attrs = append(attrs, "response_body_truncated", true)
				}
			}
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
func (s *Server) resolveGenerationReasoningEffort(ctx context.Context, model, requestedEffort string) (string, bool, error) {
	resolver, ok := s.gw.(reasoningEffortResolver)
	if !ok {
		return "", false, nil
	}
	resolvedEffort, err := resolver.ResolveReasoningEffort(ctx, model, requestedEffort, s.cfg.DefaultReasoningEffort)
	if err != nil {
		return "", true, err
	}
	return resolvedEffort, true, nil
}

// logGenerationStarted emits a dedicated log line for generation endpoints once
// the request body has been parsed and validated. Resolved reasoning effort is
// passed in from the normal request preparation path so logging does not perform
// model lookups or other gateway work by itself.
func (s *Server) logGenerationStarted(r *http.Request, endpoint, model, requestedEffort, resolvedEffort string, resolved, continuation bool) {
	meta, ok := r.Context().Value(requestLogMetadataKey{}).(*requestLogMetadata)
	if ok && meta != nil {
		meta.SetGeneration(endpoint, model, requestedEffort, resolvedEffort, resolved, continuation)
	}
	attrs := []any{
		"endpoint", endpoint,
		"model", model,
		"reasoning_effort", requestedEffort,
	}
	if continuation {
		attrs = append(attrs, "continuation", true)
	}
	if resolved && resolvedEffort != requestedEffort {
		attrs = append(attrs, "reasoning_effort_resolved", resolvedEffort)
	}
	observability.Logger(r.Context(), s.log).Info("generation started", attrs...)
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
func (m *requestLogMetadata) SetGeneration(endpoint, model, requestedEffort, resolvedEffort string, resolved bool, continuation bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.endpoint = endpoint
	m.model = model
	m.reasoningEffort = requestedEffort
	m.continuation = continuation
	if resolved {
		m.resolvedReasoningEffort = resolvedEffort
	}
}
func (m *requestLogMetadata) Fields() requestLogFields {
	m.mu.Lock()
	defer m.mu.Unlock()
	return requestLogFields{
		Endpoint:                m.endpoint,
		Model:                   m.model,
		ReasoningEffort:         m.reasoningEffort,
		ResolvedReasoningEffort: m.resolvedReasoningEffort,
		Continuation:            m.continuation,
	}
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status        int
	bytes         int
	wrote         bool
	capture       *bodyCapture
	captureActive bool
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
	if w.captureActive && w.capture != nil && n > 0 {
		w.captureActive = !w.capture.Capture(b[:n])
	}
	return n, err
}

// bodyCapture buffers up to max bytes of a stream for logging, marking
// truncated when more data was discarded.
type bodyCapture struct {
	mu        sync.Mutex
	buf       []byte
	max       int
	truncated bool
}

func newBodyCapture(max int) *bodyCapture {
	return &bodyCapture{max: max, buf: make([]byte, 0, 1024)}
}

// Capture stores as much of p as fits. It returns true once input has been
// discarded and no further captures are needed.
func (b *bodyCapture) Capture(p []byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.max - len(b.buf)
	if remaining <= 0 {
		b.truncated = true
		return true
	}
	if len(p) > remaining {
		b.buf = append(b.buf, p[:remaining]...)
		b.truncated = true
		return true
	}
	b.buf = append(b.buf, p...)
	return false
}

func (b *bodyCapture) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func (b *bodyCapture) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

// teeReadCloser copies bytes read from rc into buf, preserving Close semantics.
type teeReadCloser struct {
	rc            io.ReadCloser
	buf           *bodyCapture
	captureActive bool
}

func (t *teeReadCloser) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if t.captureActive && n > 0 {
		t.captureActive = !t.buf.Capture(p[:n])
	}
	return n, err
}

func (t *teeReadCloser) Close() error { return t.rc.Close() }
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
func boundedUserAgent(ua string) string {
	const max = 512
	if len(ua) <= max {
		return ua
	}
	return ua[:max] + "…"
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
	return openai.Internal("internal server error")
}
func errorObject(err error) openai.ErrorObject {
	api := asAPIError(err)
	return openai.ErrorObject{Message: api.Message, Type: api.Type, Param: api.Param, Code: api.Code}
}
