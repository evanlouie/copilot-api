package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/openai"
)

// This file is the single place where the transport-neutral domain error
// taxonomy (internal/apierr) is translated into HTTP. No other package may
// reference an HTTP status code.

// httpStatus maps a domain error onto its HTTP status.
func httpStatus(kind apierr.Kind) int {
	switch kind {
	case apierr.KindInvalidInput:
		return http.StatusBadRequest
	case apierr.KindUnauthorized:
		return http.StatusUnauthorized
	case apierr.KindNotFound:
		return http.StatusNotFound
	case apierr.KindTooLarge:
		return http.StatusRequestEntityTooLarge
	case apierr.KindUpstream:
		return http.StatusBadGateway
	case apierr.KindTimeout:
		return http.StatusGatewayTimeout
	case apierr.KindInternal:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

// openAIErrorType maps a domain error onto the OpenAI error-object `type`.
func openAIErrorType(kind apierr.Kind) string {
	switch kind {
	case apierr.KindUpstream, apierr.KindTimeout, apierr.KindInternal:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// domainError recovers the classified domain error, defaulting to an opaque
// internal fault so unclassified failures never leak their message.
func domainError(err error) *apierr.Error {
	var domain *apierr.Error
	if errors.As(err, &domain) {
		return domain
	}
	return apierr.Internal("internal server error")
}

func errorObject(err error) openai.ErrorObject {
	domain := domainError(err)
	return openai.ErrorObject{Message: domain.Message, Type: openAIErrorType(domain.Kind), Param: domain.Param, Code: domain.Code}
}

// WriteError renders err as an OpenAI error envelope with the mapped status.
func WriteError(w http.ResponseWriter, err error) {
	domain := domainError(err)
	writeErrorObject(w, httpStatus(domain.Kind), errorObject(err))
}

// writeErrorObject emits an error envelope with an explicit status, for the few
// failures that are purely transport-level and have no domain counterpart.
func writeErrorObject(w http.ResponseWriter, status int, obj openai.ErrorObject) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openai.ErrorEnvelope{Error: obj})
}

// jsonMuxErrors wraps a handler — in practice the ServeMux — so the plain-text
// 404 and 405 net/http generates for an unrouted request is rendered as the
// same envelope as every other error on this API.
//
// The official OpenAI SDKs parse an error body as JSON unconditionally, so
// net/http's "404 page not found" surfaces to the caller as a JSON parse error
// rather than as the 404 it is. Hitting /v1/embeddings, which this proxy does
// not implement, is the ordinary way to get there.
func jsonMuxErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&muxErrorWriter{ResponseWriter: w, req: r}, r)
	})
}

type muxErrorWriter struct {
	http.ResponseWriter
	req       *http.Request
	wrote     bool
	rewritten bool
}

// Unwrap exposes the underlying writer so http.ResponseController, the
// WebSocket upgrade's hijacker lookup and setStreamFailureWriter all still
// reach through this wrapper.
func (w *muxErrorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *muxErrorWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	// Only net/http's own defaults are rewritten. A handler that deliberately
	// produced a 404 — an unknown stored response, say — has already set a JSON
	// content type, and its body is the authoritative one.
	if obj, ok := muxDefaultError(w.req, w.Header(), status); ok && !isJSONResponse(w.Header()) {
		w.rewritten = true
		writeErrorObject(w.ResponseWriter, status, obj)
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *muxErrorWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if w.rewritten {
		// The envelope is already on the wire; discard the plain-text body
		// net/http was about to append to it.
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

func (w *muxErrorWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// muxDefaultError renders the error object for a status net/http's routing
// produced on its own. Both bodies mirror the real OpenAI API: an unknown path
// is `Invalid URL (POST /v1/embeddings)` with no param or code, and a wrong
// method is `Only POST requests are accepted.` with code method_not_supported.
func muxDefaultError(r *http.Request, header http.Header, status int) (openai.ErrorObject, bool) {
	switch status {
	case http.StatusNotFound:
		return openai.ErrorObject{Message: fmt.Sprintf("Invalid URL (%s %s)", r.Method, boundedPath(r.URL.EscapedPath())), Type: "invalid_request_error"}, true
	case http.StatusMethodNotAllowed:
		message := "Method not allowed."
		if allow := header.Get("Allow"); allow != "" {
			message = fmt.Sprintf("Only %s requests are accepted.", allow)
		}
		return openai.ErrorObject{Message: message, Type: "invalid_request_error", Code: "method_not_supported"}, true
	default:
		return openai.ErrorObject{}, false
	}
}

func isJSONResponse(header http.Header) bool {
	return strings.HasPrefix(header.Get("Content-Type"), "application/json")
}

// boundedPath caps what an unrouted path contributes to an error message, so an
// arbitrarily long URL cannot be echoed back in full.
func boundedPath(path string) string {
	const max = 256
	if len(path) <= max {
		return path
	}
	return path[:max] + "\u2026"
}
