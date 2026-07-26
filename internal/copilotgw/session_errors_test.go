package copilotgw

import (
	"errors"
	"testing"

	"github.com/evanlouie/copilot-api/internal/apierr"
	copilot "github.com/github/copilot-sdk/go"
)

func statusPtr(v int32) *int32 { return &v }

// TestUpstreamSessionErrorClassification pins how a session.error event maps
// onto the domain taxonomy, including quota - which is a 429 on the real API
// with code insufficient_quota, not a 502.
func TestUpstreamSessionErrorClassification(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		data *copilot.SessionErrorData
		kind apierr.Kind
		code string
	}{
		{"rate limit by type", &copilot.SessionErrorData{ErrorType: "rate_limit", Message: "slow down"}, apierr.KindRateLimit, "rate_limit_exceeded"},
		{"rate limit by status", &copilot.SessionErrorData{ErrorType: "query", StatusCode: statusPtr(429), Message: "slow down"}, apierr.KindRateLimit, "rate_limit_exceeded"},
		{"quota", &copilot.SessionErrorData{ErrorType: "quota", Message: "out of credits"}, apierr.KindRateLimit, "insufficient_quota"},
		{"anything else", &copilot.SessionErrorData{ErrorType: "query", Message: "bad query"}, apierr.KindUpstream, "upstream_error"},
		{"nil", nil, apierr.KindUpstream, "upstream_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := upstreamSessionError(tc.data)
			if got.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q", got.Kind, tc.kind)
			}
			if got.Code != tc.code {
				t.Fatalf("code = %q, want %q", got.Code, tc.code)
			}
		})
	}
}

// TestClassifyUpstreamErrorCoversSessionCalls covers the throttle point the
// session.error mapping cannot see.
//
// Session creation and resumption are plain RPC round trips and are where an
// aggressive rate limit lands first, but every failure there became
// apierr.Upstream - so a throttled CreateSession was reported as 502 and the
// client's 429 backoff never engaged.
func TestClassifyUpstreamErrorCoversSessionCalls(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		kind apierr.Kind
		code string
	}{
		{"jsonrpc rate limit", errors.New("JSON-RPC Error -32000: rate limit exceeded for model gpt-5"), apierr.KindRateLimit, "rate_limit_exceeded"},
		{"snake case", errors.New("JSON-RPC Error -32000: user_model_rate_limited"), apierr.KindRateLimit, "rate_limit_exceeded"},
		{"http status in the text", errors.New("upstream returned 429"), apierr.KindRateLimit, "rate_limit_exceeded"},
		{"too many requests", errors.New("Too Many Requests"), apierr.KindRateLimit, "rate_limit_exceeded"},
		{"quota", errors.New("JSON-RPC Error -32000: insufficient_quota"), apierr.KindRateLimit, "insufficient_quota"},
		{"ordinary failure", errors.New("JSON-RPC Error -32603: internal error"), apierr.KindUpstream, "upstream_error"},
		{"connection refused", errors.New("dial tcp: connection refused"), apierr.KindUpstream, "upstream_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := classifyUpstreamError(tc.err)
			if got.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q (%v)", got.Kind, tc.kind, tc.err)
			}
			if got.Code != tc.code {
				t.Fatalf("code = %q, want %q (%v)", got.Code, tc.code, tc.err)
			}
		})
	}

	// An error this proxy already classified keeps its classification rather
	// than being re-derived from its rendered message.
	original := apierr.InvalidRequest("bad tool", "tools")
	if got := classifyUpstreamError(original); got != original {
		t.Fatalf("classifyUpstreamError rewrote an already-classified error: %#v", got)
	}
	if classifyUpstreamError(nil) != nil {
		t.Fatal("classifyUpstreamError(nil) should stay nil")
	}
}
