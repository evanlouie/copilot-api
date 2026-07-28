package copilotgw

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"
)

// An external $ref is never fetched, so its strict contract can never be
// enforced. On the Chat surface that is the whole unenforceable class, since a
// freeform custom tool cannot be declared there.
func unenforceableStrictChatTool() []openai.Tool {
	return []openai.Tool{{
		Type: "function",
		Function: openai.FunctionTool{
			Name:       "lookup",
			Parameters: json.RawMessage(`{"$ref":"https://example.invalid/schema.json"}`),
			Strict:     boolPtr(true),
		},
	}}
}

func boolPtr(v bool) *bool { return &v }

// The default has to keep serving the request. Rejecting a strict tool this
// proxy merely cannot compile was tried and reverted (cd51cc7): the clients
// that set strict: true by default send these schemas, real OpenAI accepts
// them, and a 400 broke working integrations over a guarantee that is
// best-effort here regardless.
func TestBestEffortStrictEnforcementStillServesTheRequest(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("answer")}
	gw := newSDKTestGateway(t, runtime)
	gw.cfg.StrictEnforcement = config.StrictEnforcementBestEffort

	req := chatRequest("gpt-test", "hi")
	req.Tools = unenforceableStrictChatTool()
	result, err := gw.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("best-effort must accept a strict tool it cannot enforce: %v", err)
	}
	if result.Text != "answer" {
		t.Fatalf("text = %q, want the turn to have run normally", result.Text)
	}
}

// fail-closed is for the operator who would rather lose the request than serve
// a strict contract that is not being applied. The refusal has to name the tool
// and the reason, because "strict was dropped" with no subject is not something
// a client author can act on.
func TestFailClosedStrictEnforcementRefusesTheRequest(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("answer")}
	gw := newSDKTestGateway(t, runtime)
	gw.cfg.StrictEnforcement = config.StrictEnforcementFailClosed

	req := chatRequest("gpt-test", "hi")
	req.Tools = unenforceableStrictChatTool()
	_, err := gw.Chat(context.Background(), req)
	if err == nil {
		t.Fatal("fail-closed accepted a strict contract it cannot enforce")
	}
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %#v, want an API error a transport can map", err)
	}
	// A 400: the client asked for something this deployment will not serve, and
	// a retry against the same request fails identically.
	if apiErr.Kind != apierr.KindInvalidInput {
		t.Fatalf("kind = %q, want an invalid-request error", apiErr.Kind)
	}
	if apiErr.Param != "tools" {
		t.Fatalf("param = %q, want it to point at tools", apiErr.Param)
	}
	if !strings.Contains(apiErr.Message, `"lookup"`) {
		t.Fatalf("message = %q, want it to name the tool", apiErr.Message)
	}
	if !strings.Contains(apiErr.Message, "no loader") {
		t.Fatalf("message = %q, want it to carry the reason enforcement failed", apiErr.Message)
	}
	if !strings.Contains(apiErr.Message, "COPILOT_STRICT_ENFORCEMENT") {
		t.Fatalf("message = %q, want it to name the knob that produced the refusal", apiErr.Message)
	}
}

// A tuple-form items schema compiles in jsonschema-go but enforces nothing.
// Detection has to feed the same fail-closed path as an outright compile
// failure, otherwise the operator's policy can still be bypassed silently.
func TestFailClosedStrictEnforcementRefusesTupleItems(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("answer")}
	gw := newSDKTestGateway(t, runtime)
	gw.cfg.StrictEnforcement = config.StrictEnforcementFailClosed

	req := chatRequest("gpt-test", "hi")
	req.Tools = []openai.Tool{{
		Type: "function",
		Function: openai.FunctionTool{
			Name:       "lookup",
			Parameters: json.RawMessage(`{"type":"array","items":[{"type":"string"}]}`),
			Strict:     boolPtr(true),
		},
	}}
	_, err := gw.Chat(context.Background(), req)
	if err == nil {
		t.Fatal("fail-closed accepted tuple-form items as an enforceable strict contract")
	}
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Kind != apierr.KindInvalidInput {
		t.Fatalf("error = %#v, want an invalid-request error", err)
	}
	if !strings.Contains(apiErr.Message, "tuple-form `items`") {
		t.Fatalf("message = %q, want the silently ignored keyword named", apiErr.Message)
	}
}

// A strict tool whose schema does compile is enforced rather than refused, so
// fail-closed narrows what is served without rejecting strict outright.
func TestFailClosedStrictEnforcementAcceptsAnEnforceableTool(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("answer")}
	gw := newSDKTestGateway(t, runtime)
	gw.cfg.StrictEnforcement = config.StrictEnforcementFailClosed

	req := chatRequest("gpt-test", "hi")
	req.Tools = []openai.Tool{{
		Type: "function",
		Function: openai.FunctionTool{
			Name:       "lookup",
			Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`),
			Strict:     boolPtr(true),
		},
	}}
	if _, err := gw.Chat(context.Background(), req); err != nil {
		t.Fatalf("fail-closed must still serve an enforceable strict tool: %v", err)
	}
}

// The Responses surface is where a freeform custom tool can be declared, and it
// is unenforceable for a different reason: its input is a grammar in `format`,
// so there is no client-declared schema to constrain. The same policy applies.
func TestFailClosedStrictEnforcementCoversTheResponsesSurface(t *testing.T) {
	t.Parallel()
	runtime := &fakeSDKRuntime{respond: answerWith("answer")}
	gw := newSDKTestGateway(t, runtime)
	gw.cfg.StrictEnforcement = config.StrictEnforcementFailClosed

	_, err := gw.CreateResponse(context.Background(), ResponseRequest{
		Model: "gpt-test",
		Input: openai.PromptContent{Text: "the question"},
		Tools: []toolcatalog.NormalizedTool{{
			Kind:   toolcatalog.ToolKindCustom,
			Name:   "apply_patch",
			Format: json.RawMessage(`{"type":"grammar","syntax":"lark","definition":"start: /.+/"}`),
			Strict: boolPtr(true),
		}},
	})
	if err == nil {
		t.Fatal("fail-closed accepted a strict custom tool it cannot enforce")
	}
	var apiErr *apierr.Error
	if !errors.As(err, &apiErr) || apiErr.Kind != apierr.KindInvalidInput {
		t.Fatalf("error = %#v, want an invalid-request error", err)
	}
	if !strings.Contains(apiErr.Message, `"apply_patch"`) {
		t.Fatalf("message = %q, want it to name the tool", apiErr.Message)
	}
}
