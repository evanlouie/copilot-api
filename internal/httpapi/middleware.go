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

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/observability"
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
			return apierr.RequestTooLarge()
		}
		return apierr.InvalidRequest("invalid JSON request body: "+err.Error(), "body")
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return apierr.RequestTooLarge()
		}
		return apierr.InvalidRequest("request body must contain a single JSON object", "body")
	}
	return nil
}

// requestContext derives the context a handler's gateway work runs under.
//
// The returned CancelFunc is always a real one, even when no timeout is
// configured (the default). Cancellation is the only signal that releases a
// gateway producer, and on the WebSocket surface the parent is the connection
// context, which outlives every individual response: a no-op cancel there leaks
// the producer goroutine and its buffered channel until the client disconnects.
// On SSE net/http happens to cancel the request context when the handler
// returns, but relying on that makes the same call site correct on one
// transport and wrong on the other.
func requestContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(parent)
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
			WriteError(w, apierr.Unauthorized("invalid bearer token"))
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
	streamOutcome           string
	streamError             string
}

type requestLogFields struct {
	Endpoint                string
	Model                   string
	ReasoningEffort         string
	ResolvedReasoningEffort string
	Continuation            bool
	StreamOutcome           string
	StreamError             string
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
		if fields.StreamOutcome != "" {
			attrs = append(attrs, "stream_outcome", fields.StreamOutcome)
		}
		if fields.StreamError != "" {
			attrs = append(attrs, "stream_error", fields.StreamError)
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
		// A stream that fails after its headers are committed is legitimately a
		// 200: the status line is frozen and the failure is reported in-band. The
		// severity of the access record is not frozen, though, and leaving it at
		// info is what made an upstream failure storm look like healthy traffic.
		case fields.StreamOutcome == streamOutcomeFailed:
			logger.Error("request completed", attrs...)
		case fields.StreamOutcome == streamOutcomeAbandoned:
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
		StreamOutcome:           m.streamOutcome,
		StreamError:             m.streamError,
	}
}

// How a committed stream ended. Once SSE headers are on the wire the HTTP
// status is frozen at 200 and the failure has to be reported in-band, so this
// is the only thing that distinguishes a healthy turn from an upstream failure
// in the access log.
const (
	// streamOutcomeCompleted: the terminal frame was written.
	streamOutcomeCompleted = "completed"
	// streamOutcomeFailed: the turn failed and the failure was reported in-band.
	streamOutcomeFailed = "failed"
	// streamOutcomeAbandoned: writing stopped without a terminal frame, almost
	// always because the client went away mid-stream.
	streamOutcomeAbandoned = "abandoned"
)

func (m *requestLogMetadata) SetStreamOutcome(outcome string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streamOutcome = outcome
	if err != nil {
		m.streamError = err.Error()
	} else {
		m.streamError = ""
	}
}

func (m *requestLogMetadata) SetStreamOutcomeIfUnset(outcome string, err error) {
	m.mu.Lock()
	unset := m.streamOutcome == ""
	m.mu.Unlock()
	if unset {
		m.SetStreamOutcome(outcome, err)
	}
}

func requestLogMetadataFrom(r *http.Request) *requestLogMetadata {
	meta, ok := r.Context().Value(requestLogMetadataKey{}).(*requestLogMetadata)
	if !ok {
		return nil
	}
	return meta
}

// recordStreamOutcome puts the terminal outcome of a committed stream on the
// access line and emits a dedicated record for it. Severity follows the
// outcome, not the (frozen) HTTP status.
func (s *Server) recordStreamOutcome(ctx context.Context, r *http.Request, endpoint, outcome string, streamErr error) {
	if meta := requestLogMetadataFrom(r); meta != nil {
		meta.SetStreamOutcome(outcome, streamErr)
	}
	attrs := []any{"endpoint", endpoint, "stream_outcome", outcome}
	if streamErr != nil {
		attrs = append(attrs, "stream_error", streamErr.Error())
	}
	logger := observability.Logger(ctx, s.log)
	switch outcome {
	case streamOutcomeFailed:
		logger.Error("stream ended", attrs...)
	case streamOutcomeAbandoned:
		logger.Warn("stream ended", attrs...)
	default:
		logger.Debug("stream ended", attrs...)
	}
}

// markStreamAbandoned is the fallback for the write-error return paths, which
// stop mid-stream without a terminal frame. It records nothing when an outcome
// is already known and emits no record of its own: an access line that says
// "abandoned" is the whole signal.
func markStreamAbandoned(r *http.Request) {
	if meta := requestLogMetadataFrom(r); meta != nil {
		meta.SetStreamOutcomeIfUnset(streamOutcomeAbandoned, nil)
	}
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status        int
	bytes         int
	wrote         bool
	capture       *bodyCapture
	captureActive bool
	// streamFailure terminates an already-committed stream with an in-band
	// failure frame. Streaming handlers register it; only recoverMiddleware,
	// running on the same goroutine while the handler unwinds, reads it.
	streamFailure func(error)
}

// committedResponseWriter is implemented by loggingResponseWriter so
// recoverMiddleware can tell whether bytes are already on the wire and, when a
// streaming handler registered one, how to terminate that stream in-band.
type committedResponseWriter interface {
	Committed() bool
	StreamFailure() func(error)
}

// Committed reports whether the status line has already been sent. Once it has,
// status and headers are frozen and anything further must speak the body
// grammar the client is already parsing.
func (w *loggingResponseWriter) Committed() bool { return w.wrote }

func (w *loggingResponseWriter) StreamFailure() func(error) { return w.streamFailure }

// setStreamFailureWriter records how to terminate an already-committed stream.
// Streaming handlers call it once their SSE writer exists, so a panic unwinding
// past the point where an HTTP error response is still possible can still be
// reported to the client. It unwraps, because the handler is handed whatever
// middleware sits between it and the recorder.
func setStreamFailureWriter(w http.ResponseWriter, fail func(error)) {
	for {
		if recorder, ok := w.(*loggingResponseWriter); ok {
			recorder.streamFailure = fail
			return
		}
		unwrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return
		}
		w = unwrapper.Unwrap()
	}
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
			v := recover()
			if v == nil {
				return
			}
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				// net/http suppresses this panic deliberately, so re-panic
				// rather than inventing a 500 for a connection a lower layer
				// intentionally aborted.
				panic(v)
			}
			logger := observability.Logger(r.Context(), log)
			logger.Error("panic in HTTP handler", "panic", v, "stack", string(debug.Stack()))
			writePanicFailure(logger, w)
		}()
		next.ServeHTTP(w, r)
	})
}

// writePanicFailure reports a recovered panic using whatever the response state
// still allows.
func writePanicFailure(logger *slog.Logger, w http.ResponseWriter) {
	failure := apierr.Internal("internal server error")
	committed, ok := w.(committedResponseWriter)
	if !ok || !committed.Committed() {
		WriteError(w, failure)
		return
	}
	// The status line is already on the wire: WriteError's header work would be a
	// silent no-op and its JSON body would be appended verbatim to the committed
	// body, where SSE clients discard it as a line with no known field name.
	fail := committed.StreamFailure()
	if fail == nil {
		logger.Warn("panic after response commit; client sees a truncated response")
		return
	}
	fail(failure)
}
