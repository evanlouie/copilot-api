package copilotgw

import (
	"testing"

	"github.com/evanlouie/copilot-api/internal/apierr"
	copilot "github.com/github/copilot-sdk/go"
)

func statusPtr(v int32) *int32 { return &v }

// A throttled turn used to reach the client as a generic upstream fault, which
// the transport renders as 502 server_error. The official OpenAI SDKs back off
// on 429 and retry a 502 on a generic schedule, so the classification decides
// whether a client waits out a Copilot rate limit or hammers through it.
func TestUpstreamSessionErrorClassifiesRateLimits(t *testing.T) {
	for _, test := range []struct {
		name string
		data *copilot.SessionErrorData
		kind apierr.Kind
		code string
	}{
		{
			name: "rate_limit error type",
			data: &copilot.SessionErrorData{ErrorType: "rate_limit", Message: "You have exceeded your premium request allowance."},
			kind: apierr.KindRateLimit,
			code: "rate_limit_exceeded",
		},
		{
			name: "upstream 429 without the category",
			data: &copilot.SessionErrorData{ErrorType: "query", Message: "too many requests", StatusCode: statusPtr(429)},
			kind: apierr.KindRateLimit,
			code: "rate_limit_exceeded",
		},
		{
			name: "quota stays an upstream failure",
			data: &copilot.SessionErrorData{ErrorType: "quota", Message: "billing not configured"},
			kind: apierr.KindUpstream,
			code: "upstream_error",
		},
		{
			name: "ordinary session error",
			data: &copilot.SessionErrorData{ErrorType: "query", Message: "boom"},
			kind: apierr.KindUpstream,
			code: "upstream_error",
		},
		{
			name: "upstream 500 is not a rate limit",
			data: &copilot.SessionErrorData{ErrorType: "query", Message: "boom", StatusCode: statusPtr(500)},
			kind: apierr.KindUpstream,
			code: "upstream_error",
		},
		{
			name: "nil event",
			data: nil,
			kind: apierr.KindUpstream,
			code: "upstream_error",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := upstreamSessionError(test.data)
			if err.Kind != test.kind || err.Code != test.code {
				t.Fatalf("upstreamSessionError = %#v, want kind %q code %q", err, test.kind, test.code)
			}
			if test.data != nil && test.data.Message != "" && err.Message != test.data.Message {
				t.Fatalf("message = %q, want the upstream message %q", err.Message, test.data.Message)
			}
		})
	}
}

// The SDK exposes no retry-after on a session error, so the wait is genuinely
// unknown and must stay unset rather than be invented.
func TestUpstreamRateLimitCarriesNoInventedRetryAfter(t *testing.T) {
	err := upstreamSessionError(&copilot.SessionErrorData{ErrorType: "rate_limit", Message: "slow down"})
	if err.RetryAfter != 0 {
		t.Fatalf("RetryAfter = %s, want 0: the Copilot SDK does not report one on this event", err.RetryAfter)
	}
}
