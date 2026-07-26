package copilotgw

import (
	"errors"
	"strings"

	"github.com/evanlouie/copilot-api/internal/apierr"
	copilot "github.com/github/copilot-sdk/go"
)

// upstreamRateLimitStatus is the HTTP status the Copilot backend uses for
// throttling. It is compared as a plain integer on purpose: this package is
// transport-neutral and must not import net/http.
const upstreamRateLimitStatus = 429

// The Copilot SDK documents SessionErrorData.ErrorType as one of
// "authentication", "authorization", "quota", "rate_limit", "context_limit" or
// "query", and names the fine-grained codes that accompany a throttle
// (user_weekly_rate_limited, user_global_rate_limited, rate_limited,
// user_model_rate_limited, integration_rate_limited).
const (
	sdkRateLimitErrorType = "rate_limit"
	sdkQuotaErrorType     = "quota"
)

// upstreamSessionError classifies a session.error event from the Copilot SDK.
//
// Throttling used to degrade to a generic upstream fault, which the transport
// renders as 502 server_error. That matters: the official OpenAI SDKs
// special-case 429 and back off on it, and they retry a 502 on a generic
// schedule instead - against a backend that rate-limits as aggressively as
// GitHub Copilot, that is the difference between waiting out a limit and
// hammering through it.
//
// The SDK exposes no retry-after on this event. The only RetryAfterSeconds it
// carries is on AutoModeSwitchRequestedData, a separate prompt asking the user
// to switch models, which this proxy does not consume - so the wait is left
// unset and the client falls back to its own backoff schedule.
func upstreamSessionError(d *copilot.SessionErrorData) *apierr.Error {
	if d == nil {
		return apierr.Upstream("copilot session error")
	}
	if d.ErrorType == sdkRateLimitErrorType || (d.StatusCode != nil && *d.StatusCode == upstreamRateLimitStatus) {
		return apierr.RateLimited(d.Message, 0)
	}
	// Quota exhaustion is also a 429 on the real API, with code
	// insufficient_quota rather than rate_limit_exceeded. It was previously kept
	// at 502 on the theory that retrying cannot clear a billing block - but that
	// argument inverts itself: the official SDKs retry 5xx on their generic
	// schedule, so reporting 502 produced *more* automatic retries than the 429
	// it was avoiding, and told the client the wrong thing about why.
	if d.ErrorType == sdkQuotaErrorType {
		return apierr.QuotaExhausted(d.Message)
	}
	return apierr.Upstream(d.Message)
}

// requestToolsError classifies a failure from building a request-scoped tool
// catalog.
//
// Catalog construction now validates two request fields at once: the tools
// themselves and the tool_choice that narrows them. Everything it returns used
// to be blamed on "tools", which for a forced tool_choice naming a tool the
// request never declared would send the client looking in the wrong field, so
// an already-classified failure keeps the param it named.
func requestToolsError(err error) error {
	var domain *apierr.Error
	if errors.As(err, &domain) {
		return domain
	}
	return apierr.InvalidRequest(err.Error(), "tools")
}

// classifyUpstreamError maps an error returned by an SDK call - as opposed to a
// session.error event - onto the domain taxonomy.
//
// Session creation and resumption are plain RPC round trips to the Copilot
// backend, and they are the first place an aggressive rate limit lands. Every
// such failure used to become apierr.Upstream, so a throttled CreateSession was
// reported as 502 server_error and the client's 429 backoff never engaged.
//
// This is deliberately a string match, which is a heuristic and is worth being
// explicit about. The SDK returns *internal/jsonrpc2.Error, whose package is
// unexported, so nothing outside the SDK can type-assert it or read its Code
// and Data fields; err.Error() renders as "JSON-RPC Error %d: %s" and that
// formatted string is the only signal available. The needles below are
// distinctive enough that a false positive is unlikely, and the cost of one is
// a client backing off when it did not need to - strictly better than the false
// negative that is today's unconditional 502.
func classifyUpstreamError(err error) *apierr.Error {
	if err == nil {
		return nil
	}
	// Anything already classified keeps its classification.
	var domain *apierr.Error
	if errors.As(err, &domain) {
		return domain
	}
	message := err.Error()
	lowered := strings.ToLower(message)
	for _, needle := range []string{"rate limit", "rate_limit", "too many requests", "429"} {
		if strings.Contains(lowered, needle) {
			return apierr.RateLimited(message, 0)
		}
	}
	for _, needle := range []string{"insufficient_quota", "quota exceeded", "quota_exceeded"} {
		if strings.Contains(lowered, needle) {
			return apierr.QuotaExhausted(message)
		}
	}
	return apierr.Upstream(message)
}
