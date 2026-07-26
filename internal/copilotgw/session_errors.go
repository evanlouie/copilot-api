package copilotgw

import (
	"github.com/evanlouie/copilot-api/internal/apierr"
	copilot "github.com/github/copilot-sdk/go"
)

// upstreamRateLimitStatus is the HTTP status the Copilot backend uses for
// throttling. It is compared as a plain integer on purpose: this package is
// transport-neutral and must not import net/http.
const upstreamRateLimitStatus = 429

// sdkRateLimitErrorType is the value the Copilot SDK puts in
// SessionErrorData.ErrorType when a request was throttled. The SDK documents
// the enum on that field ("authentication", "authorization", "quota",
// "rate_limit", "context_limit", "query") and names the fine-grained codes that
// accompany it (user_weekly_rate_limited, user_global_rate_limited,
// rate_limited, user_model_rate_limited, integration_rate_limited).
const sdkRateLimitErrorType = "rate_limit"

// upstreamSessionError classifies a session.error event from the Copilot SDK.
//
// Throttling used to degrade to a generic upstream fault, which the transport
// renders as 502 server_error. That matters: the official OpenAI SDKs
// special-case 429 and back off on it, and they retry a 502 on a generic
// schedule instead — against a backend that rate-limits as aggressively as
// GitHub Copilot, that is the difference between waiting out a limit and
// hammering through it.
//
// The SDK exposes no retry-after on this event. The only RetryAfterSeconds it
// carries is on AutoModeSwitchRequestedData, a separate prompt asking the user
// to switch models, which this proxy does not consume — so the wait is left
// unset and the client falls back to its own backoff schedule.
//
// Only rate_limit is mapped. The SDK's "quota" category is also a 429 on the
// real OpenAI API (insufficient_quota), but no amount of retrying clears a
// billing block, so it deliberately keeps reporting as an upstream failure.
func upstreamSessionError(d *copilot.SessionErrorData) *apierr.Error {
	if d == nil {
		return apierr.Upstream("copilot session error")
	}
	if d.ErrorType == sdkRateLimitErrorType || (d.StatusCode != nil && *d.StatusCode == upstreamRateLimitStatus) {
		return apierr.RateLimited(d.Message, 0)
	}
	return apierr.Upstream(d.Message)
}
