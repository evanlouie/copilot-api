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
// The schemas that reach this path are ones real OpenAI accepts, and the
// external $ref is this proxy's own limitation on purpose: Resolve is called
// with no loader because fetching a schema a request names is an SSRF
// primitive. Since the clients that set strict: true by default send these, a
// 400 would break working integrations over a guarantee that is best-effort
// here in the first place - unless the operator asks for one, which
// TestFailClosedStrictEnforcementRefusesTheRequest covers.
func TestStrictToolWithUnusableSchemaIsAcceptedAndReported(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		schema string
	}{
		{"unresolvable local ref", `{"type":"object","properties":{"city":{"$ref":"#/nope/missing"}}}`},
		{"external ref", `{"$ref":"https://example.invalid/schema.json"}`},
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

// jsonschema-go compiles Draft-04/07 tuple-form items but applies none of the
// positional schemas. A strict request must therefore be reported as
// unenforceable instead of looking successfully constrained.
func TestTupleItemsStrictSchemaIsAcceptedAndReported(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		schema string
	}{
		{name: "top level", schema: `{"type":"array","items":[{"type":"string"}]}`},
		{name: "nested through properties", schema: `{"type":"object","properties":{"pair":{"type":"array","items":[{"type":"string"},{"type":"number"}]}}}`},
		{name: "nested through composition", schema: `{"allOf":[{"type":"object","properties":{"pair":{"type":"array","items":[{"type":"string"}]}}}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", tc.schema, boolPtr(true))}, openai.ToolScope{})
			if err != nil {
				t.Fatalf("tuple-form items should follow best-effort strict handling: %v", err)
			}
			if len(rt.UnenforceableStrict) != 1 {
				t.Fatalf("UnenforceableStrict = %#v, want the tuple schema reported", rt.UnenforceableStrict)
			}
			if reason := rt.UnenforceableStrict[0].Reason; !strings.Contains(reason, "tuple-form `items`") || !strings.Contains(reason, "does not enforce") {
				t.Fatalf("reason = %q, want the silent under-enforcement named", reason)
			}
			// Best-effort still offers the tool, but with no validator attached. The
			// important invariant is that this is explicitly reported rather than
			// represented as an enforced strict contract.
			if err := validateStrictArguments(rt.client["lookup"], json.RawMessage(`[123]`)); err != nil {
				t.Fatalf("best-effort tuple schema unexpectedly installed a validator: %v", err)
			}
		})
	}
}

// Tuple detection follows schema positions only. Client data may contain an
// object named items without declaring the tuple keyword.
func TestTupleItemsDetectionLeavesSchemaDataAlone(t *testing.T) {
	t.Parallel()
	const schema = `{"type":"object","properties":{"config":{"type":"object","default":{"items":[{"type":"string"}]}}}}`
	rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", schema, boolPtr(true))}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.UnenforceableStrict) != 0 {
		t.Fatalf("UnenforceableStrict = %#v, want data under default ignored", rt.UnenforceableStrict)
	}
}

// The external $ref stays unresolved on purpose, so pin that the reason is a
// compile failure and not a fetch. Handing a loader to Resolve would turn any
// client that can name a URL into an SSRF primitive against this process's
// network position, which is not a trade a best-effort guarantee justifies.
func TestExternalRefIsReportedWithoutBeingFetched(t *testing.T) {
	t.Parallel()
	// A host that cannot resolve: if this were ever fetched the test would stall
	// or fail on the network rather than pass instantly.
	rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", `{"$ref":"https://example.invalid/schema.json"}`, boolPtr(true))}, openai.ToolScope{})
	if err != nil {
		t.Fatalf("an external $ref must not fail the request: %v", err)
	}
	if len(rt.UnenforceableStrict) != 1 {
		t.Fatalf("UnenforceableStrict = %#v, want the unfetched $ref reported", rt.UnenforceableStrict)
	}
	if reason := rt.UnenforceableStrict[0].Reason; !strings.Contains(reason, "no loader") {
		t.Fatalf("reason = %q, want it to say the schema was never loaded", reason)
	}
}

// Draft-04 spells an exclusive bound as a boolean flag beside `minimum`, which
// the Draft 2020-12 model cannot hold, so these tools used to be accepted
// unenforced purely because of a gap in this proxy's compiler. They are
// rewritten into the numeric 2020-12 form and enforced for real.
//
// Enforcement, not just compilation, is what the assertions pin: a rewrite that
// dropped the bound would compile happily and quietly enforce nothing, which is
// the exact failure this is meant to close.
func TestLegacyExclusiveBoundsAreCompiledAndEnforced(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		schema   string
		accepted string
		refused  string
	}{
		{
			name:     "exclusiveMinimum true excludes the bound",
			schema:   `{"type":"object","properties":{"n":{"type":"number","minimum":1,"exclusiveMinimum":true}}}`,
			accepted: `{"n":1.5}`,
			refused:  `{"n":1}`,
		},
		{
			name:     "exclusiveMaximum true excludes the bound",
			schema:   `{"type":"object","properties":{"n":{"type":"number","maximum":10,"exclusiveMaximum":true}}}`,
			accepted: `{"n":9}`,
			refused:  `{"n":10}`,
		},
		{
			// A false flag is Draft-04 for "inclusive", so the bound itself has to
			// survive the rewrite rather than being dropped along with the flag.
			name:     "exclusiveMinimum false keeps the inclusive bound",
			schema:   `{"type":"object","properties":{"n":{"type":"number","minimum":1,"exclusiveMinimum":false}}}`,
			accepted: `{"n":1}`,
			refused:  `{"n":0.5}`,
		},
		{
			// The flag can sit anywhere a schema can, so the rewrite has to reach
			// through $defs and the composition keywords, not just top-level
			// properties.
			name:     "nested through $defs and allOf",
			schema:   `{"type":"object","$defs":{"N":{"type":"number","minimum":1,"exclusiveMinimum":true}},"properties":{"n":{"allOf":[{"$ref":"#/$defs/N"}]}}}`,
			accepted: `{"n":2}`,
			refused:  `{"n":1}`,
		},
		{
			// A schema list is a position the walk has to treat as schemas rather
			// than as data. Draft-07's tuple form of `items` would be the obvious
			// case, but jsonschema-go enforces nothing for it either way, so
			// prefixItems is what can actually be asserted.
			name:     "nested in a prefixItems list",
			schema:   `{"type":"object","properties":{"pair":{"type":"array","prefixItems":[{"type":"number","minimum":1,"exclusiveMinimum":true}]}}}`,
			accepted: `{"pair":[2]}`,
			refused:  `{"pair":[1]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", tc.schema, boolPtr(true))}, openai.ToolScope{})
			if err != nil {
				t.Fatal(err)
			}
			if len(rt.UnenforceableStrict) != 0 {
				t.Fatalf("UnenforceableStrict = %#v, want a legacy exclusive bound to be enforceable", rt.UnenforceableStrict)
			}
			if err := validateStrictArguments(rt.client["lookup"], json.RawMessage(tc.accepted)); err != nil {
				t.Fatalf("validate(%s) = %v, want conforming arguments to pass", tc.accepted, err)
			}
			if err := validateStrictArguments(rt.client["lookup"], json.RawMessage(tc.refused)); err == nil {
				t.Fatalf("validate(%s) = nil, want the rewritten bound to still constrain", tc.refused)
			}
		})
	}
}

// The rewrite walks keywords, not every map it can reach, because a schema can
// legitimately contain an object with an `exclusiveMinimum` member that is data
// rather than a keyword. Rewriting one of those would change what the client
// declared.
func TestLegacyExclusiveBoundRewriteLeavesDataAlone(t *testing.T) {
	t.Parallel()
	// A property literally named exclusiveMinimum, plus a default whose value
	// carries the same member name.
	const schema = `{"type":"object","properties":{"exclusiveMinimum":{"type":"boolean"},"opts":{"type":"object","default":{"exclusiveMinimum":true,"minimum":1}}},"required":["exclusiveMinimum"]}`
	rt, err := NewRequestTools(NewBroker(time.Minute), []openai.Tool{strictChatTool(t, "lookup", schema, boolPtr(true))}, openai.ToolScope{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rt.UnenforceableStrict) != 0 {
		t.Fatalf("UnenforceableStrict = %#v, want empty", rt.UnenforceableStrict)
	}
	if err := validateStrictArguments(rt.client["lookup"], json.RawMessage(`{"exclusiveMinimum":true}`)); err != nil {
		t.Fatalf("validate = %v, want a property named exclusiveMinimum to be read as a property", err)
	}
	if err := validateStrictArguments(rt.client["lookup"], json.RawMessage(`{"exclusiveMinimum":"yes"}`)); err == nil {
		t.Fatal("a property named exclusiveMinimum must still be type-checked")
	}
	// The declared parameters are what the SDK is handed, so the rewrite must
	// never be visible there either.
	props, _ := rt.client["lookup"].Parameters["properties"].(map[string]any)
	opts, _ := props["opts"].(map[string]any)
	dflt, _ := opts["default"].(map[string]any)
	if _, ok := dflt["minimum"]; !ok {
		t.Fatalf("default = %#v, want the client's value untouched", dflt)
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
