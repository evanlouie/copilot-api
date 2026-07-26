// Package apierr defines the transport-neutral error taxonomy shared by the
// domain layer (request validation, the Copilot gateway, the tool catalog) and
// the transport layer.
//
// Nothing here knows about HTTP. A domain package classifies a failure by Kind
// and supplies the operator/client-facing message plus the OpenAI `param` and
// `code` hints; the transport layer decides what that means on the wire. The
// HTTP mapping lives in exactly one place, internal/httpapi.
package apierr

import (
	"fmt"
	"time"
)

// Kind classifies a failure. It is the only classification the domain layer
// produces; transports derive their own vocabulary (HTTP status, OpenAI error
// `type`, ...) from it.
type Kind string

const (
	// KindInternal is the zero value: an unclassified server-side fault.
	KindInternal Kind = "internal"
	// KindInvalidInput means the caller sent something this API cannot accept.
	KindInvalidInput Kind = "invalid_input"
	// KindUnauthorized means the caller could not be authenticated.
	KindUnauthorized Kind = "unauthorized"
	// KindNotFound means the addressed resource does not exist.
	KindNotFound Kind = "not_found"
	// KindTooLarge means the caller exceeded a size limit.
	KindTooLarge Kind = "too_large"
	// KindUpstream means a dependency (the Copilot SDK) failed.
	KindUpstream Kind = "upstream"
	// KindTimeout means the operation ran out of time.
	KindTimeout Kind = "timeout"
	// KindRateLimit means an upstream throttle rejected the request. It is kept
	// distinct from KindUpstream because clients act on it: the official OpenAI
	// SDKs implement backoff keyed on it and honour the wait the server names.
	KindRateLimit Kind = "rate_limit"
	// KindUnavailable means this service cannot accept the request right now,
	// through no fault of the caller or of a dependency — a shutdown in progress,
	// say. Retrying later is the correct response, which is not true of
	// KindUpstream.
	KindUnavailable Kind = "unavailable"
)

// Error is a classified domain failure. Param and Code carry the OpenAI error
// object hints so a transport can render them verbatim without having to
// re-derive them from the message.
type Error struct {
	Kind    Kind
	Message string
	Param   string
	Code    string
	// RetryAfter is how long the caller should wait before retrying, when the
	// failure named a wait. Zero means none was given. Transports render it in
	// whatever way they express "try again later".
	RetryAfter time.Duration
}

func (e *Error) Error() string { return e.Message }

// InvalidRequest reports caller input this API cannot accept, blaming param.
func InvalidRequest(message, param string) *Error {
	return &Error{Kind: KindInvalidInput, Message: message, Param: param}
}

// RequestTooLarge reports a body over the configured size limit.
func RequestTooLarge() *Error {
	return &Error{Kind: KindTooLarge, Message: "request body exceeds the configured size limit", Param: "body", Code: "request_too_large"}
}

// Unauthorized reports a missing or unusable credential.
func Unauthorized(message string) *Error {
	return &Error{Kind: KindUnauthorized, Message: message, Code: "invalid_api_key"}
}

// NotFound reports an addressed resource that does not exist.
func NotFound(message, code string) *Error {
	return &Error{Kind: KindNotFound, Message: message, Code: code}
}

// PreviousResponseNotFound reports an unresolvable previous_response_id. It is
// invalid input rather than a missing resource: the request itself is at fault.
func PreviousResponseNotFound(id string) *Error {
	return &Error{Kind: KindInvalidInput, Message: fmt.Sprintf("Previous response with id %q not found.", id), Param: "previous_response_id", Code: "previous_response_not_found"}
}

// Upstream reports a failure originating in the Copilot SDK.
func Upstream(message string) *Error {
	return &Error{Kind: KindUpstream, Message: message, Code: "upstream_error"}
}

// RateLimited reports that an upstream throttle rejected the request.
// retryAfter is the wait the upstream named, or zero when it named none.
func RateLimited(message string, retryAfter time.Duration) *Error {
	if message == "" {
		message = "rate limit reached"
	}
	return &Error{Kind: KindRateLimit, Message: message, Code: "rate_limit_exceeded", RetryAfter: retryAfter}
}

// QuotaExhausted reports that the caller's upstream allowance is spent. It is
// a rate limit on the wire - the real API answers 429 - but carries its own
// code, because the remedy is billing rather than waiting.
func QuotaExhausted(message string) *Error {
	if message == "" {
		message = "insufficient quota"
	}
	return &Error{Kind: KindRateLimit, Message: message, Code: "insufficient_quota"}
}

// Unavailable reports that this service cannot take the request right now.
func Unavailable(message string) *Error {
	return &Error{Kind: KindUnavailable, Message: message, Code: "service_unavailable"}
}

// Timeout reports an operation that ran out of time.
func Timeout() *Error {
	return &Error{Kind: KindTimeout, Message: "request timed out", Code: "request_timeout"}
}

func Internal(message string) *Error {
	return &Error{Kind: KindInternal, Message: message, Code: "internal_error"}
}
