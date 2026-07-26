package toolproxy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/openai"
)

// TestStrictArgumentFailureIsClassified pins that a strict-schema violation
// reaches the client as something it can act on.
//
// The failure used to be a bare errors.New. Through CaptureRequests it was
// wrapped as apierr.Upstream(err.Error()) - a 502 server_error indistinguishable
// from a network fault, which the official SDKs retry on their generic 5xx
// schedule against a turn that will fail identically. Through handleInvocation
// it fell through domainError's fallback and reached the client as a bare 500
// "internal server error" with the tool name gone.
func TestStrictArgumentFailureIsClassified(t *testing.T) {
	t.Parallel()
	rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", strictLookupSchema, boolPtr(true))}, false)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args string
	}{
		{"wrong type", `{"city":123}`},
		{"missing required", `{}`},
		{"not json at all", `{nope`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateStrictArguments(rt.client["lookup"], json.RawMessage(tc.args))
			if err == nil {
				t.Fatal("strict arguments that violate the schema must be refused")
			}
			if !errors.Is(err, errStrictArguments) {
				t.Fatalf("error = %v, want it to wrap errStrictArguments", err)
			}

			// The classification has to survive unwrapping, which is what both
			// materialisation paths do before the error reaches a transport.
			var domain *apierr.Error
			if !errors.As(err, &domain) {
				t.Fatalf("error = %v, want an *apierr.Error a transport can map", err)
			}
			if domain.Code != "strict_tool_arguments_invalid" {
				t.Fatalf("code = %q, want it distinguishable from a generic upstream failure", domain.Code)
			}
			if domain.Kind != apierr.KindUpstream {
				t.Fatalf("kind = %q, want upstream: the model produced the bad arguments", domain.Kind)
			}
			// The tool name is the one thing a client needs to act on.
			if !strings.Contains(domain.Message, `"lookup"`) {
				t.Fatalf("message = %q, want it to name the tool", domain.Message)
			}
		})
	}
}
