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
	rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", strictLookupSchema, boolPtr(true))}, openai.ToolScope{})
	if err != nil {
		t.Fatalf("strict tool should be accepted: %v", err)
	}
	if len(rt.Tools()) != 1 {
		t.Fatalf("SDK tools = %#v, want one", rt.Tools())
	}
}

// A strict tool whose schema this proxy cannot compile is accepted and reported
// rather than rejected.
//
// The schemas that reach this path are ones real OpenAI accepts: Draft-07's
// boolean exclusiveMinimum, which jsonschema-go does not model, and any
// external $ref, which fails because Resolve is called with no loader - this
// proxy's own limitation, not something the client did. Since the clients that
// set strict: true by default send exactly these, a 400 would break working
// integrations over a guarantee that is best-effort here in the first place.
func TestStrictToolWithUnusableSchemaIsAcceptedAndReported(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		schema string
	}{
		{"unresolvable local ref", `{"type":"object","properties":{"city":{"$ref":"#/nope/missing"}}}`},
		{"external ref", `{"$ref":"https://example.invalid/schema.json"}`},
		{"draft-07 exclusiveMinimum", `{"type":"object","properties":{"n":{"minimum":1,"exclusiveMinimum":true}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", tc.schema, boolPtr(true))}, openai.ToolScope{})
			if err != nil {
				t.Fatalf("uncompilable strict schema should not fail the request: %v", err)
			}
			if len(rt.Tools()) != 1 {
				t.Fatalf("SDK tools = %#v, want the tool to still be offered", rt.Tools())
			}
			if len(rt.UnenforceableStrict) != 1 {
				t.Fatalf("UnenforceableStrict = %#v, want the acceptance to be reported", rt.UnenforceableStrict)
			}
			if got := rt.UnenforceableStrict[0].Tool; got != "lookup" {
				t.Fatalf("reported tool = %q, want %q", got, "lookup")
			}
			if rt.UnenforceableStrict[0].Reason == "" {
				t.Fatal("reported an unenforceable strict tool with no reason")
			}
			// Unenforced means unenforced: arguments the schema would have
			// rejected must now be delivered rather than failing the turn.
			if err := validateStrictArguments(rt.client["lookup"], json.RawMessage(`{"city":123}`)); err != nil {
				t.Fatalf("validate = %v, want no enforcement for an uncompilable schema", err)
			}
		})
	}
}

// A tool whose schema does compile is still enforced, so the fallback above is
// scoped to schemas this proxy genuinely cannot read.
func TestStrictToolWithUsableSchemaIsStillEnforced(t *testing.T) {
	t.Parallel()
	rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", strictLookupSchema, boolPtr(true))}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.UnenforceableStrict) != 0 {
		t.Fatalf("UnenforceableStrict = %#v, want empty for a compilable schema", rt.UnenforceableStrict)
	}
	if err := validateStrictArguments(rt.client["lookup"], json.RawMessage(`{"city":123}`)); err == nil {
		t.Fatal("a compilable strict schema must still reject non-conforming arguments")
	}
}

// The same schema without strict: true is not compiled at all, because nothing
// is promised about the arguments.
func TestNonStrictToolKeepsUnusableSchema(t *testing.T) {
	t.Parallel()
	if _, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", `{"type":"object","properties":{"city":{"$ref":"#/nope/missing"}}}`, nil)}, openai.ToolScope{}); err != nil {
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
			rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", strictLookupSchema, boolPtr(true))}, openai.ToolScope{})
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
	rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", strictLookupSchema, nil)}, openai.ToolScope{})
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
	rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", strictLookupSchema, boolPtr(true))}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	handler := rt.Tools()[0].Handler
	if _, err := handler(copilot.ToolInvocation{ToolCallID: "call_1", Arguments: map[string]any{"city": 7}}); !errors.Is(err, errStrictArguments) {
		t.Fatalf("handler error = %v, want a strict-argument refusal", err)
	}
}

// A freeform custom tool has no client-declared JSON Schema to constrain, so
// strict cannot be enforced against the synthetic schema this proxy hands the
// SDK. Like an uncompilable schema, that is reported rather than rejected.
func TestStrictCustomToolIsAcceptedAndReported(t *testing.T) {
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
	rt, err := newRequestToolsFromClientTools(NewBroker(time.Minute), tools, openai.ToolScope{})
	if err != nil {
		t.Fatalf("strict on a custom tool should not fail the request: %v", err)
	}
	if len(rt.UnenforceableStrict) != 1 || !strings.Contains(rt.UnenforceableStrict[0].Reason, "format") {
		t.Fatalf("UnenforceableStrict = %#v, want one entry naming the freeform input", rt.UnenforceableStrict)
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
	rt, err := newRequestToolsFromClientTools(NewBroker(time.Minute), tools, openai.ToolScope{})
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
			}}, openai.ToolScope{})
			if err != nil {
				t.Fatal(err)
			}
			if got := rt.Tools()[0].Defer; got != tt.want {
				t.Fatalf("Defer = %q, want %q", got, tt.want)
			}
		})
	}
}
