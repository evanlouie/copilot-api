package toolproxy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"

	copilot "github.com/github/copilot-sdk/go"
)

func boolPtr(v bool) *bool { return &v }

func strictChatTool(t *testing.T, name, schema string, strict *bool) openai.Tool {
	t.Helper()
	return openai.Tool{
		Type: "function",
		Function: openai.FunctionTool{
			Name:       name,
			Parameters: json.RawMessage(schema),
			Strict:     strict,
		},
	}
}

const strictLookupSchema = `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`

// strict: true has to keep returning 200 for the request itself - it is what
// Vercel AI SDK, LangChain and the OpenAI Agents SDK send by default - so the
// guarantee is enforced against the arguments instead.
func TestStrictToolRequestIsAccepted(t *testing.T) {
	t.Parallel()
	rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", strictLookupSchema, boolPtr(true))}, false)
	if err != nil {
		t.Fatalf("strict tool should be accepted: %v", err)
	}
	if len(rt.Tools()) != 1 {
		t.Fatalf("SDK tools = %#v, want one", rt.Tools())
	}
}

// A strict tool whose schema cannot be compiled can never have its guarantee
// enforced, so it fails at request time rather than mid-turn.
func TestStrictToolWithUnusableSchemaIsRejected(t *testing.T) {
	t.Parallel()
	_, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", `{"type":"object","properties":{"city":{"$ref":"#/nope/missing"}}}`, boolPtr(true))}, false)
	if err == nil {
		t.Fatal("expected an unusable strict schema to be rejected")
	}
	if !strings.Contains(err.Error(), "strict: true") {
		t.Fatalf("error = %v, want it to name the strict declaration", err)
	}
}

// The same schema without strict: true is not compiled at all, because nothing
// is promised about the arguments.
func TestNonStrictToolKeepsUnusableSchema(t *testing.T) {
	t.Parallel()
	if _, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", `{"type":"object","properties":{"city":{"$ref":"#/nope/missing"}}}`, nil)}, false); err != nil {
		t.Fatalf("a non-strict tool must not be schema-checked: %v", err)
	}
}

func TestStrictArgumentsAreValidatedBeforeReachingTheClient(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		arguments map[string]any
		wantErr   bool
	}{
		{name: "conforming", arguments: map[string]any{"city": "lisbon"}},
		{name: "wrong type", arguments: map[string]any{"city": 7}, wantErr: true},
		{name: "missing required", arguments: map[string]any{}, wantErr: true},
		{name: "extra property", arguments: map[string]any{"city": "lisbon", "unit": "c"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", strictLookupSchema, boolPtr(true))}, false)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: "lookup", Arguments: tt.arguments}}, "resp_1", "chat", "gpt-test", make(chan TurnFinalResult, 1), nil)
			if tt.wantErr {
				if !errors.Is(err, errStrictArguments) {
					t.Fatalf("CaptureRequests error = %v, want a strict-argument refusal", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("conforming arguments were refused: %v", err)
			}
		})
	}
}

// Without strict: true the same arguments pass straight through: the client
// asked for nothing, so nothing is enforced.
func TestNonStrictArgumentsAreNotValidated(t *testing.T) {
	t.Parallel()
	rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", strictLookupSchema, nil)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := rt.CaptureRequests([]copilot.AssistantMessageToolRequest{{ToolCallID: "call_1", Name: "lookup", Arguments: map[string]any{"nonsense": true}}}, "resp_1", "chat", "gpt-test", make(chan TurnFinalResult, 1), nil); err != nil {
		t.Fatalf("non-strict arguments should not be validated: %v", err)
	}
}

// The SDK handler is the other way a call is materialized, so it enforces the
// same guarantee rather than letting handler-first ordering slip past.
func TestStrictArgumentsAreValidatedInTheSDKHandler(t *testing.T) {
	t.Parallel()
	rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", strictLookupSchema, boolPtr(true))}, false)
	if err != nil {
		t.Fatal(err)
	}
	handler := rt.Tools()[0].Handler
	if _, err := handler(copilot.ToolInvocation{ToolCallID: "call_1", Arguments: map[string]any{"city": 7}}); !errors.Is(err, errStrictArguments) {
		t.Fatalf("handler error = %v, want a strict-argument refusal", err)
	}
}

// A freeform custom tool has no client-declared JSON Schema to constrain, so
// strict is refused instead of being checked against the synthetic schema this
// proxy hands the SDK.
func TestStrictCustomToolIsRejected(t *testing.T) {
	t.Parallel()
	tools, err := FlattenResponsesTools([]toolcatalog.NormalizedTool{{
		Kind:   toolcatalog.ToolKindCustom,
		Name:   "apply_patch",
		Format: json.RawMessage(`{"type":"grammar","syntax":"lark","definition":"start: /.+/"}`),
		Strict: boolPtr(true),
	}})
	if err != nil {
		t.Fatalf("flatten: %v", err)
	}
	if _, err := newRequestToolsFromClientTools(NewBroker(time.Minute), tools, false); err == nil || !strings.Contains(err.Error(), "freeform") {
		t.Fatalf("error = %v, want a refusal naming the freeform input", err)
	}
}

// A namespace does not survive flattening, so the defer_loading it declared has
// to travel with the children that replace it.
func TestNamespaceDeferLoadingReachesItsChildren(t *testing.T) {
	t.Parallel()
	tools, err := FlattenResponsesTools([]toolcatalog.NormalizedTool{{
		Kind:         toolcatalog.ToolKindNamespace,
		Name:         "mcp__grep_app",
		DeferLoading: boolPtr(true),
		Children: []toolcatalog.NormalizedTool{
			{Kind: toolcatalog.ToolKindFunction, Name: "searchGitHub", Parameters: json.RawMessage(`{"type":"object"}`)},
			{Kind: toolcatalog.ToolKindFunction, Name: "listRepos", Parameters: json.RawMessage(`{"type":"object"}`), DeferLoading: boolPtr(false)},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	rt, err := newRequestToolsFromClientTools(NewBroker(time.Minute), tools, false)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]copilot.ToolDefer{}
	for _, tool := range rt.Tools() {
		got[tool.Name] = tool.Defer
	}
	if len(got) != 2 {
		t.Fatalf("SDK tools = %#v, want two", got)
	}
	for name, defer_ := range got {
		want := copilot.ToolDeferAuto
		if strings.Contains(name, "listRepos") {
			want = copilot.ToolDeferNever
		}
		if defer_ != want {
			t.Fatalf("%s Defer = %q, want %q", name, defer_, want)
		}
	}
}

// defer_loading is a Copilot-side concept the SDK does honour, so it is
// forwarded rather than dropped.
func TestDeferLoadingIsForwardedToTheSDK(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		defer_ *bool
		want   copilot.ToolDefer
	}{
		{name: "absent leaves the runtime's choice", defer_: nil, want: ""},
		{name: "true asks for lazy loading", defer_: boolPtr(true), want: copilot.ToolDeferAuto},
		{name: "false pins the tool preloaded", defer_: boolPtr(false), want: copilot.ToolDeferNever},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rt, err := newRequestToolsFromClientTools(NewBroker(time.Minute), []ClientTool{{
				SDKName:      "lookup",
				ResponseName: "lookup",
				Parameters:   map[string]any{"type": "object"},
				DeferLoading: tt.defer_,
			}}, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := rt.Tools()[0].Defer; got != tt.want {
				t.Fatalf("Defer = %q, want %q", got, tt.want)
			}
		})
	}
}
