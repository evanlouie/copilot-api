package toolproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evanlouie/copilot-api/internal/apierr"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/toolcatalog"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
)

const NoToolsSentinel = toolcatalog.NoToolsSentinelName

var (
	ErrExpired  = errors.New("pending tool call batch expired")
	ErrNotFound = errors.New("pending tool call batch not found")
)

type ClientTool struct {
	SDKName      string
	ResponseKind toolcatalog.ResponsesToolKind
	ResponseName string
	Namespace    string
	Description  string
	Parameters   map[string]any
	Strict       *bool
	DeferLoading *bool
	Execution    string

	// strictSchema is the compiled form of Parameters, present only when the
	// client asked for strict: true. It is what makes strict mean something here:
	// see validateStrictArguments.
	strictSchema *jsonschema.Resolved
}

// strictEnabled reports whether the client asked for schema-constrained
// arguments on this tool.
func (t ClientTool) strictEnabled() bool { return t.Strict != nil && *t.Strict }

// deferMode maps the OpenAI/Copilot `defer_loading` flag onto the SDK's tool
// deferral control. The SDK honours copilot.Tool.Defer, so the flag is forwarded
// rather than dropped: true asks for lazy loading through tool search, false
// asks for the tool to always be pre-loaded, and an absent flag leaves the
// runtime's own choice alone.
func (t ClientTool) deferMode() copilot.ToolDefer {
	if t.DeferLoading == nil {
		return ""
	}
	if *t.DeferLoading {
		return copilot.ToolDeferAuto
	}
	return copilot.ToolDeferNever
}

// resolveStrictSchema compiles a strict tool's declared parameters so the
// arguments the model emits can be checked against them.
//
// A schema this proxy cannot compile is NOT a client error, and returning 400
// for one by default was wrong. The cases that reach it are ones real OpenAI
// accepts:
//
//   - Legacy exclusive bounds. `{"exclusiveMinimum": true}` alongside `minimum`
//     is the Draft-04 spelling and fails to unmarshal here, because
//     jsonschema-go models the Draft 2020-12 numeric form. These are now
//     rewritten and compiled - see withNumericExclusiveBounds - so they reach
//     this path only when something else about the schema is also unreadable.
//   - Any external `$ref`. Resolve is called with no loader, so
//     `{"$ref":"https://..."}` fails with "cannot resolve remote schemas".
//     That is this proxy's own limitation, not something the client did, and it
//     stays that way deliberately: fetching a schema named by a request at
//     request time is an SSRF primitive, and no guarantee is worth handing the
//     model's caller an outbound request of its choosing.
//
// The clients that set strict: true by default - the Vercel AI SDK,
// LangChain/LangGraph, the OpenAI Agents SDK, Cline - would have had working
// integrations broken outright by a 400 they cannot act on. So an
// uncompilable schema degrades to unenforced, which is exactly the position
// every client was in before strict was honoured at all, and the reason is
// reported so it is visible rather than silent. An operator who would rather
// fail than serve an unenforced contract sets COPILOT_STRICT_ENFORCEMENT to
// fail-closed; the gateway turns the same report into a 400 there.
//
// The second return value is empty when strict is either enforced or not
// requested, and otherwise says why it could not be enforced.
func (t ClientTool) resolveStrictSchema() (*jsonschema.Resolved, string) {
	if !t.strictEnabled() {
		return nil, ""
	}
	// A freeform custom tool describes its input with a grammar in `format`, not
	// a JSON Schema, and this proxy hands the SDK a synthetic single-string
	// schema for it. There is nothing client-declared to constrain.
	if t.ResponseKind == toolcatalog.ToolKindCustom {
		return nil, "a custom tool declares its input with `format`, which cannot be schema-constrained"
	}
	resolved, unenforceable := compileStrictSchema(t.Parameters)
	if unenforceable == "" {
		return resolved, ""
	}
	// Retrying only after a failure is what keeps the rewrite from being a
	// behaviour change: a schema that compiles today compiles from exactly the
	// bytes the client sent, and the rewritten copy is never handed to the SDK.
	if rewritten, rewrote := withNumericExclusiveBounds(t.Parameters); rewrote {
		if retried, retryReason := compileStrictSchema(rewritten); retryReason == "" {
			return retried, ""
		}
	}
	// Reported as the original failure, not the retry's: the first attempt is the
	// one that describes what the client actually sent.
	return nil, unenforceable
}

// compileStrictSchema compiles declared parameters into a validator, returning
// the reason it could not rather than an error, because every caller reports it
// as prose alongside the tool name.
func compileStrictSchema(params map[string]any) (*jsonschema.Resolved, string) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, "its parameters cannot be encoded: " + err.Error()
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return nil, "its parameters are not a JSON Schema this proxy can compile: " + err.Error()
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return nil, "its parameters are not a JSON Schema this proxy can compile: " + err.Error()
	}
	return resolved, ""
}

// exclusiveBoundPairs are Draft-04's boolean exclusive-bound flags paired with
// the inclusive bound each one modifies.
var exclusiveBoundPairs = [...]struct{ exclusive, inclusive string }{
	{"exclusiveMinimum", "minimum"},
	{"exclusiveMaximum", "maximum"},
}

// The keywords the rewrite descends through. Walking only positions that hold
// schemas is what stops it reaching client data: `default`, `const`, `enum` and
// `examples` can each contain an object with an `exclusiveMinimum` member that
// is a value rather than a keyword, and rewriting one of those would corrupt
// the constraint instead of translating it.
var (
	// subschemaKeywords hold a single schema, except `items`, which holds an
	// ordered list in the Draft-04/07 tuple form and a single schema in 2020-12.
	subschemaKeywords = [...]string{
		"additionalItems", "additionalProperties", "contains", "contentSchema",
		"else", "if", "items", "not", "propertyNames", "then",
		"unevaluatedItems", "unevaluatedProperties",
	}
	// subschemaListKeywords hold an ordered list of schemas.
	subschemaListKeywords = [...]string{"allOf", "anyOf", "oneOf", "prefixItems"}
	// subschemaMapKeywords hold schemas under names the client chose, so their
	// keys are never keywords. `dependencies` is the Draft-07 form whose values
	// are either a schema or a list of property names; a list is left alone.
	subschemaMapKeywords = [...]string{
		"$defs", "definitions", "dependencies", "dependentSchemas",
		"patternProperties", "properties",
	}
)

// withNumericExclusiveBounds returns a copy of a schema with Draft-04's boolean
// exclusive bounds rewritten into the numeric form Draft 2020-12 uses, plus
// whether anything was rewritten.
//
// `{"minimum": 1, "exclusiveMinimum": true}` means x > 1 in Draft-04 and
// `{"exclusiveMinimum": 1}` means x > 1 in 2020-12, so the translation is exact
// rather than an approximation. A false flag simply drops out, leaving the
// inclusive bound, and a flag with no bound beside it carried no constraint in
// Draft-04 either, so it is dropped entirely.
//
// This is deliberately a rewrite rather than a multi-draft compiler.
// santhosh-tekuri/jsonschema/v6 was measured against these exact cases and is
// worse on all of them: under its 2020-12 default the boolean form still fails
// (so does declaring `$schema` as Draft-07, whose own metaschema already
// requires a numeric exclusiveMinimum - the boolean spelling is Draft-04), and
// it additionally rejects tuple `items` and `additionalItems`, which
// jsonschema-go accepts today. Forcing its default draft to 4 fixes the boolean
// form by breaking the numeric one, which is the spelling almost every client
// sends. There is no single draft that reads both, so the choice is a rewrite
// or a heuristic that guesses a draft per schema - and the rewrite is the
// smaller, checkable one, with no new dependency.
//
// Values are left as they were decoded, which is json.Number, so a bound moved
// between keywords keeps the precision the client sent it with.
func withNumericExclusiveBounds(schema map[string]any) (map[string]any, bool) {
	out := make(map[string]any, len(schema))
	for key, value := range schema {
		out[key] = value
	}
	rewrote := false
	for _, pair := range exclusiveBoundPairs {
		flag, isBool := out[pair.exclusive].(bool)
		if !isBool {
			continue
		}
		rewrote = true
		delete(out, pair.exclusive)
		if bound, ok := out[pair.inclusive]; ok && flag {
			out[pair.exclusive] = bound
			delete(out, pair.inclusive)
		}
	}
	for _, keyword := range subschemaKeywords {
		switch child := out[keyword].(type) {
		case map[string]any:
			if rewritten, changed := withNumericExclusiveBounds(child); changed {
				out[keyword], rewrote = rewritten, true
			}
		case []any:
			if rewritten, changed := rewriteSubschemaList(child); changed {
				out[keyword], rewrote = rewritten, true
			}
		}
	}
	for _, keyword := range subschemaListKeywords {
		if child, ok := out[keyword].([]any); ok {
			if rewritten, changed := rewriteSubschemaList(child); changed {
				out[keyword], rewrote = rewritten, true
			}
		}
	}
	for _, keyword := range subschemaMapKeywords {
		child, ok := out[keyword].(map[string]any)
		if !ok {
			continue
		}
		named := make(map[string]any, len(child))
		changedAny := false
		for name, sub := range child {
			named[name] = sub
			subSchema, ok := sub.(map[string]any)
			if !ok {
				continue
			}
			if rewritten, changed := withNumericExclusiveBounds(subSchema); changed {
				named[name], changedAny = rewritten, true
			}
		}
		if changedAny {
			out[keyword], rewrote = named, true
		}
	}
	return out, rewrote
}

func rewriteSubschemaList(items []any) ([]any, bool) {
	out := make([]any, len(items))
	rewrote := false
	for i, item := range items {
		out[i] = item
		sub, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if rewritten, changed := withNumericExclusiveBounds(sub); changed {
			out[i], rewrote = rewritten, true
		}
	}
	return out, rewrote
}

// errStrictArguments is returned when the model emits arguments that do not
// satisfy a strict tool's declared schema.
//
// It is wrapped in an *apierr.Error rather than left bare because a bare error
// loses everything the client needs. Through CaptureRequests it became
// apierr.Upstream(err.Error()) - a 502 server_error indistinguishable from a
// network fault, which the official SDKs retry on their 5xx schedule against a
// turn that will fail identically. Through handleInvocation it fell to
// domainError's fallback and reached the client as a bare 500 "internal server
// error" with the tool name gone entirely.
//
// The kind stays upstream, because the model is what produced the bad
// arguments and a re-run genuinely might not, but the distinct code and the
// preserved message let a client tell the two apart.
var errStrictArguments = errors.New("strict tool arguments did not match the declared schema")

// strictArgumentsError classifies a strict-schema violation so it survives
// both materialisation paths with its tool name and reason intact.
func strictArgumentsError(toolName, detail string) error {
	message := fmt.Sprintf("tool %q emitted arguments that do not satisfy the strict schema it declared: %s", toolName, detail)
	return fmt.Errorf("%w: %w", errStrictArguments, &apierr.Error{
		Kind:    apierr.KindUpstream,
		Message: message,
		Code:    "strict_tool_arguments_invalid",
	})
}

// validateStrictArguments is where strict: true stops being decoration. The
// Copilot SDK exposes no constrained-decoding control (copilot.Tool carries a
// name, a description, parameters, a defer mode and a handler, and nothing that
// bounds the model's output), so the guarantee OpenAI provides by construction
// is provided here by refusing to hand the client a call that does not satisfy
// the schema it declared. A client that sets strict: true and skips its own
// validation - which is the entire point of setting it - never sees arguments
// this proxy could not verify.
//
// The refusal is deliberate rather than a retry: this proxy does not own the
// model's decoding loop, so the honest outcome is an explicit failure naming
// the tool, not a silently different call.
func validateStrictArguments(meta ClientTool, args json.RawMessage) error {
	if meta.strictSchema == nil {
		return nil
	}
	var instance any
	if len(args) == 0 {
		instance = map[string]any{}
	} else if err := json.Unmarshal(args, &instance); err != nil {
		// Deliberately a plain Unmarshal, not the UseNumber decode schemaMap uses
		// for the schema side. jsonschema-go reflects over the instance and types
		// a json.Number as "string", so decoding with UseNumber makes every
		// numeric constraint fail: measured against v0.4.3, {"n":5} against
		// {"type":"integer"} reports `5 has type "string", want "integer"`.
		// Both the schema's literals and the instance round through float64 here,
		// so they round identically and compare correctly - the consistency that
		// matters for validation is between these two, not with the bytes on the
		// wire, which schemaMap already preserves separately.
		return strictArgumentsError(meta.ResponseName, "they are not valid JSON: "+err.Error())
	}
	if err := meta.strictSchema.Validate(instance); err != nil {
		return strictArgumentsError(meta.ResponseName, err.Error())
	}
	return nil
}

type CapturedCall struct {
	Kind          toolcatalog.ResponsesToolKind
	SDKName       string
	ResponseName  string
	Namespace     string
	CallID        string
	ArgumentsJSON json.RawMessage
	Input         string
	Execution     string
}

type Broker struct {
	mu      sync.Mutex
	batches map[string]*Batch
	byCall  map[string]*Batch
	ttl     time.Duration
	closed  bool
}

func NewBroker(ttl time.Duration) *Broker {
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	return &Broker{batches: map[string]*Batch{}, byCall: map[string]*Batch{}, ttl: ttl}
}

// Register makes batch reachable by its call ids and arranges for the broker to
// forget it when it closes.
//
// Register is called once by CaptureRequests and again by every
// handleInvocation, so an N-tool turn registers the same batch N+1 times.
// Registration is idempotent, and removal is scheduled by binding the broker to
// the batch rather than by appending a hook: a field holds one broker, so
// "remove exactly once" is a property of the structure and not of how carefully
// callers count their Register calls.
func (b *Broker) Register(batch *Batch) {
	if !batch.bindBroker(b) {
		// The batch closed before this registration, so its close has already run
		// and will not run again. Drop any lookup entries it still owns here.
		b.Remove(batch)
		return
	}
	if !batch.isOpen() {
		return
	}
	// Snapshot before taking b.mu: the batch's call map is guarded by batch.mu,
	// which SDK tool handlers write to concurrently with expiry-driven removal.
	ids := batch.callIDs()
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		batch.Cancel(context.Canceled)
		return
	}
	defer b.mu.Unlock()
	b.batches[batch.ID] = batch
	for _, id := range ids {
		b.byCall[id] = batch
	}
}

func (b *Broker) FindByCallIDs(ids []string) (*Batch, error) {
	found, matched, err := b.findByCallIDs(ids, true)
	if err != nil {
		return nil, err
	}
	if len(matched) == 0 {
		return nil, ErrNotFound
	}
	return found, nil
}

// FindByAnyCallIDs returns the single live batch referenced by any of ids, plus
// the subset of ids that belong to it. Missing ids are ignored. If ids point to
// multiple live batches, the request is ambiguous and an error is returned.
func (b *Broker) FindByAnyCallIDs(ids []string) (*Batch, []string, error) {
	return b.findByCallIDs(ids, false)
}

func (b *Broker) findByCallIDs(ids []string, requireAll bool) (*Batch, []string, error) {
	b.mu.Lock()
	var found *Batch
	matched := make([]string, 0, len(ids))
	stale := make([]*Batch, 0)
	missingRequired := false
	for _, id := range ids {
		batch := b.byCall[id]
		if batch == nil {
			if requireAll {
				missingRequired = true
				break
			}
			continue
		}
		if !batch.isOpen() {
			stale = append(stale, batch)
			if requireAll {
				missingRequired = true
				break
			}
			continue
		}
		if found != nil && found.ID != batch.ID {
			b.mu.Unlock()
			return nil, nil, fmt.Errorf("tool_call_ids belong to different pending batches")
		}
		found = batch
		matched = append(matched, id)
	}
	b.mu.Unlock()
	for _, batch := range stale {
		b.Remove(batch)
	}
	if missingRequired || found == nil {
		return nil, nil, ErrNotFound
	}
	return found, matched, nil
}

func (b *Broker) Remove(batch *Batch) {
	ids := batch.callIDs()
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.batches, batch.ID)
	for _, id := range ids {
		// Only drop lookup entries this batch still owns. A later batch may have
		// re-registered the same id, and orphaning its entry would strand a live
		// batch that continuations can no longer find.
		if b.byCall[id] == batch {
			delete(b.byCall, id)
		}
	}
}

func (b *Broker) CancelAll(err error) {
	if b == nil {
		return
	}
	b.mu.Lock()
	batches := make([]*Batch, 0, len(b.batches))
	for _, batch := range b.batches {
		batches = append(batches, batch)
	}
	b.batches = map[string]*Batch{}
	b.byCall = map[string]*Batch{}
	b.closed = true
	b.mu.Unlock()
	for _, batch := range batches {
		batch.Cancel(err)
	}
}

type RequestTools struct {
	broker    *Broker
	permitted map[string]struct{}
	client    map[string]ClientTool
	tools     []copilot.Tool
	available []string
	mu        sync.Mutex
	ctx       context.Context
	batch     *Batch
	// reserved holds the client-visible identity minted for a tool call whose
	// argument fragments are being streamed, keyed by the model's tool-call id.
	// It exists because a streamed fragment has to name the call before the SDK
	// announces the finished tool request that ensureCall would normally mint
	// the id from.
	reserved map[string]StreamingCall

	// UnenforceableStrict lists tools whose strict: true was accepted but cannot
	// be enforced, with the reason. Read once at request setup by the gateway,
	// which owns the logger; never mutated afterwards, so it needs no lock.
	UnenforceableStrict []UnenforceableStrictTool
}

// UnenforceableStrictTool records a tool that asked for strict: true which this
// proxy accepted but cannot enforce, so the gateway can report it.
type UnenforceableStrictTool struct {
	Tool   string
	Reason string
}

// StreamingCall is the client-visible identity of a tool call whose arguments
// may be forwarded fragment by fragment, before the call itself exists.
type StreamingCall struct {
	CallID string
	Kind   toolcatalog.ResponsesToolKind
	Name   string
}

// ReserveStreamingCall decides whether a tool call's arguments may be streamed
// as the model produces them, and pins the identity such a stream must use.
//
// The gate is strict tools, and it is the whole reason this is a decision
// rather than a lookup. validateStrictArguments refuses to hand the client a
// strict call whose arguments do not satisfy the declared schema, and that
// refusal can only be made once the arguments are complete. Streaming
// fragments of a strict call would publish arguments this proxy may be about
// to reject, which is exactly the guarantee strict: true buys. Strict calls
// therefore stay buffered; everything else streams.
//
// The reserved id matters as much as the decision. ensureCall normally mints
// the client-visible id when the SDK announces the finished tool request, but
// a streamed fragment names its call long before that. Minting here and
// consuming the reservation there is what makes the fragments and the final
// tool call the same call rather than two.
//
// custom reports the SDK's tool-call type discriminator, so the kind returned
// here is the same kind CaptureRequests will settle on.
func (rt *RequestTools) ReserveStreamingCall(sdkID, sdkName string, custom bool) (StreamingCall, bool) {
	// An empty id cannot be correlated back to a tool request, so a fragment
	// carrying one could never be reconciled against a final call.
	if rt == nil || sdkID == "" || sdkName == "" {
		return StreamingCall{}, false
	}
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if call, ok := rt.reserved[sdkID]; ok {
		return call, true
	}
	meta, ok := rt.clientTool(sdkName)
	if !ok || meta.strictEnabled() {
		return StreamingCall{}, false
	}
	kind := meta.ResponseKind
	if kind == "" {
		kind = toolcatalog.ToolKindFunction
	}
	if custom && kind == toolcatalog.ToolKindFunction {
		kind = toolcatalog.ToolKindCustom
	}
	name := meta.ResponseName
	if name == "" {
		name = sdkName
	}
	call := StreamingCall{CallID: "call_" + uuid.NewString(), Kind: kind, Name: name}
	if rt.reserved == nil {
		rt.reserved = map[string]StreamingCall{}
	}
	rt.reserved[sdkID] = call
	return call, true
}

// takeReservedCallIDLocked consumes the id reserved for a streamed tool call.
// Consuming rather than reading keeps the map bounded by the calls still in
// flight instead of by every call the request ever made.
func (rt *RequestTools) takeReservedCallIDLocked(sdkID string) string {
	call, ok := rt.reserved[sdkID]
	if !ok {
		return ""
	}
	delete(rt.reserved, sdkID)
	return call.CallID
}

func NewRequestTools(broker *Broker, tools []openai.Tool, scope openai.ToolScope) (*RequestTools, error) {
	tools = openai.SupportedTools(tools)
	clientTools := make([]ClientTool, 0, len(tools))
	for _, t := range tools {
		params, err := schemaMap(t.Function.Name, t.Function.Parameters)
		if err != nil {
			return nil, err
		}
		clientTools = append(clientTools, ClientTool{SDKName: t.Function.Name, ResponseKind: toolcatalog.ToolKindFunction, ResponseName: t.Function.Name, Description: t.Function.Description, Parameters: params, Strict: t.Function.Strict})
	}
	return newRequestToolsFromClientTools(broker, clientTools, scope)
}

func NewResponseRequestTools(broker *Broker, tools []toolcatalog.NormalizedTool, scope openai.ToolScope) (*RequestTools, error) {
	clientTools, err := FlattenResponsesTools(tools)
	if err != nil {
		return nil, err
	}
	return newRequestToolsFromClientTools(broker, clientTools, scope)
}

func newRequestToolsFromClientTools(broker *Broker, clientTools []ClientTool, scope openai.ToolScope) (*RequestTools, error) {
	clientTools, err := scopeClientTools(clientTools, scope)
	if err != nil {
		return nil, err
	}
	rt := &RequestTools{broker: broker, permitted: map[string]struct{}{}, client: map[string]ClientTool{}, ctx: context.Background()}
	if scope.None || len(clientTools) == 0 {
		rt.available = []string{NoToolsSentinel}
		return rt, nil
	}
	available := copilot.NewToolSet()
	for _, ct := range clientTools {
		if ct.SDKName == "" {
			return nil, fmt.Errorf("tool has empty SDK name")
		}
		if _, exists := rt.client[ct.SDKName]; exists {
			return nil, fmt.Errorf("duplicate SDK tool name %q", ct.SDKName)
		}
		strictSchema, unenforceable := ct.resolveStrictSchema()
		if unenforceable != "" {
			// Recorded rather than logged here: this package owns no logger, and
			// the gateway that does is where every other per-request note is
			// written. Reporting it is what keeps "accepted but not honoured"
			// from being silent.
			rt.UnenforceableStrict = append(rt.UnenforceableStrict, UnenforceableStrictTool{Tool: ct.ResponseName, Reason: unenforceable})
		}
		ct.strictSchema = strictSchema
		rt.client[ct.SDKName] = ct
		rt.permitted[ct.SDKName] = struct{}{}
		ctCopy := ct
		rt.tools = append(rt.tools, copilot.Tool{
			Name:                 ct.SDKName,
			Description:          ct.Description,
			Parameters:           ct.Parameters,
			OverridesBuiltInTool: true,
			Defer:                ct.deferMode(),
			Handler: func(inv copilot.ToolInvocation) (copilot.ToolResult, error) {
				inv.ToolName = ctCopy.SDKName
				return rt.handleInvocation(inv)
			},
		})
		available.AddCustom(ct.SDKName)
	}
	rt.available = available.ToSlice()
	return rt, nil
}

// scopeClientTools narrows the request-scoped catalog to the tools a
// tool_choice permits.
//
// This is the only enforcement lever there is. The Copilot SDK has no
// tool_choice concept, so what the model may call is decided entirely by what
// it is shown, and the same AvailableTools narrowing that implements
// tool_choice: "none" implements the rest. For allowed_tools that is the whole
// of the semantic and the result is exact. For a forced function/custom choice
// it is not: the model is left able to call nothing but the named tool, which
// still leaves it free to answer in prose instead of calling anything.
//
// Filtering runs after flattening so SDK names, aliases and collision checks
// are computed over the catalog the client actually declared - a narrowed
// request must not rename or start accepting tools that a wider one rejected.
func scopeClientTools(tools []ClientTool, scope openai.ToolScope) ([]ClientTool, error) {
	if scope.None || len(scope.Only) == 0 {
		return tools, nil
	}
	allowed := make(map[string]struct{}, len(scope.Only))
	for _, name := range scope.Only {
		allowed[name] = struct{}{}
	}
	kept := make([]ClientTool, 0, len(tools))
	for _, tool := range tools {
		for _, name := range clientToolChoiceNames(tool) {
			if _, ok := allowed[name]; ok {
				kept = append(kept, tool)
				break
			}
		}
	}
	if scope.Forced && len(kept) == 0 {
		// OpenAI rejects a forced choice for a tool the request did not declare,
		// and so must this: narrowing to nothing would hand the model an empty
		// catalog and no way at all to do what the client asked for. An
		// allow-list is a filter by nature and gets no such treatment - a
		// Responses catalog can still grow through tool_search, so a name that
		// matches nothing yet is not necessarily a client mistake.
		return nil, apierr.InvalidRequest(fmt.Sprintf("tool_choice names tool %q, which is not in this request's tool catalog", scope.Only[0]), "tool_choice")
	}
	return kept, nil
}

// clientToolChoiceNames lists the spellings a tool_choice may use to name this
// tool. Clients address tools by the name they declared them under, never by
// the SDK alias this proxy mints, and a namespace child answers to both its
// bare name and the dotted canonical form its output items carry.
func clientToolChoiceNames(tool ClientTool) []string {
	if tool.Namespace == "" {
		return []string{tool.ResponseName}
	}
	return []string{tool.ResponseName, tool.Namespace + "." + tool.ResponseName}
}

func FlattenResponsesTools(tools []toolcatalog.NormalizedTool) ([]ClientTool, error) {
	flattened := make([]ClientTool, 0, len(tools))
	for _, tool := range tools {
		switch tool.Kind {
		case toolcatalog.ToolKindFunction:
			ct, err := clientToolFromNormalized(tool, "")
			if err != nil {
				return nil, err
			}
			flattened = append(flattened, ct)
		case toolcatalog.ToolKindCustom:
			ct, err := clientToolFromNormalized(tool, "")
			if err != nil {
				return nil, err
			}
			flattened = append(flattened, ct)
		case toolcatalog.ToolKindNamespace:
			for _, child := range tool.Children {
				child.Namespace = tool.Name
				// The namespace is what the client asked to defer, and the namespace
				// does not survive flattening, so its flag has to travel with the
				// children that replace it. A child that states its own preference
				// keeps it.
				if child.DeferLoading == nil {
					child.DeferLoading = tool.DeferLoading
				}
				ct, err := clientToolFromNormalized(child, tool.Name)
				if err != nil {
					return nil, err
				}
				flattened = append(flattened, ct)
			}
		case toolcatalog.ToolKindToolSearch:
			ct, err := clientToolFromNormalized(tool, "")
			if err != nil {
				return nil, err
			}
			flattened = append(flattened, ct)
		}
	}
	return assignSDKNames(flattened)
}

func clientToolFromNormalized(tool toolcatalog.NormalizedTool, namespace string) (ClientTool, error) {
	if namespace == "" {
		namespace = tool.Namespace
	}
	var params map[string]any
	var err error
	switch tool.Kind {
	case toolcatalog.ToolKindCustom:
		params = customToolSchema(tool.Name)
	case toolcatalog.ToolKindToolSearch:
		params, err = schemaMap(tool.Name, tool.Parameters)
	default:
		params, err = schemaMap(tool.Name, tool.Parameters)
	}
	if err != nil {
		return ClientTool{}, err
	}
	desc := tool.Description
	ct := ClientTool{ResponseKind: tool.Kind, ResponseName: tool.Name, Namespace: namespace, Description: desc, Parameters: params, Strict: tool.Strict, DeferLoading: tool.DeferLoading, Execution: tool.Execution}
	if ct.ResponseKind == toolcatalog.ToolKindToolSearch && ct.Execution == "" {
		ct.Execution = "client"
	}
	return ct, nil
}

func assignSDKNames(tools []ClientTool) ([]ClientTool, error) {
	used := map[string]string{NoToolsSentinel: "reserved sentinel"}
	out := make([]ClientTool, len(tools))
	for i, tool := range tools {
		identity := responseIdentity(tool)
		if prior, ok := used[identity]; ok {
			return nil, fmt.Errorf("duplicate Responses tool identity %q conflicts with %s", identity, prior)
		}
		used[identity] = "Responses identity"
		out[i] = tool
	}
	used = map[string]string{NoToolsSentinel: "reserved sentinel"}
	for i := range out {
		name := desiredSDKName(out[i])
		if prior, ok := used[name]; ok {
			return nil, fmt.Errorf("SDK tool name collision for %q with %s", name, prior)
		}
		used[name] = responseIdentity(out[i])
		out[i].SDKName = name
		out[i].Description = descriptionWithCanonicalName(out[i])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SDKName < out[j].SDKName })
	return out, nil
}

func desiredSDKName(tool ClientTool) string {
	return toolcatalog.NormalizedToolSDKName(toolcatalog.NormalizedTool{Kind: tool.ResponseKind, Name: tool.ResponseName, Namespace: tool.Namespace})
}

func responseIdentity(tool ClientTool) string {
	if tool.Namespace != "" {
		return string(tool.ResponseKind) + ":" + tool.Namespace + "." + tool.ResponseName
	}
	return string(tool.ResponseKind) + ":" + tool.ResponseName
}

func descriptionWithCanonicalName(tool ClientTool) string {
	canonical := tool.ResponseName
	if tool.Namespace != "" {
		canonical = tool.Namespace + "." + tool.ResponseName
	}
	prefix := "Responses tool " + canonical + "."
	if tool.ResponseKind == toolcatalog.ToolKindCustom {
		prefix += " Freeform custom tool; provide the raw tool input in the required JSON string field named input."
	}
	if tool.ResponseKind == toolcatalog.ToolKindToolSearch {
		prefix += " Client-executed tool discovery; returns loadable client tool specs."
	}
	if strings.TrimSpace(tool.Description) == "" {
		return prefix
	}
	return prefix + " " + tool.Description
}

// schemaMap decodes a client-supplied JSON Schema so it can be handed to the
// SDK, which re-encodes it for the model.
//
// UseNumber is load-bearing, not a micro-optimization. Without it every JSON
// number in the schema becomes a float64 and is re-marshalled from that
// float64, so {"maximum":1e21} reaches the model as 1e+21 and
// {"enum":[9007199254740993]} as 9007199254740992. The model must see the
// schema the client wrote. toolcatalog.CanonicalRawJSON decodes the same
// documents the same way for the same reason.
func schemaMap(toolName string, raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var decoded any
	if err := dec.Decode(&decoded); err != nil {
		return nil, invalidToolSchema(toolName, "is not valid JSON: "+err.Error())
	}
	// Decoder.Decode, unlike json.Unmarshal, stops at the end of the first value.
	// Reject anything after it so malformed input cannot be silently truncated.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, invalidToolSchema(toolName, "has trailing data after the JSON Schema object")
	}
	params, ok := decoded.(map[string]any)
	if !ok {
		// A raw encoding/json error ("json: cannot unmarshal bool into Go value of
		// type map[string]interface {}") describes this proxy's internals, not the
		// client's mistake, and would be classified as a server fault.
		return nil, invalidToolSchema(toolName, "must be a JSON Schema object")
	}
	return params, nil
}

func invalidToolSchema(toolName, problem string) error {
	if toolName == "" {
		return apierr.InvalidRequest("tool parameters "+problem, "tools")
	}
	return apierr.InvalidRequest(fmt.Sprintf("tool %q parameters %s", toolName, problem), "tools")
}

func customToolSchema(name string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"input": map[string]any{
				"type":        "string",
				"description": "Raw freeform input for the Responses custom tool " + name + ".",
			},
		},
		"required":             []any{"input"},
		"additionalProperties": false,
	}
}

func (rt *RequestTools) Tools() []copilot.Tool    { return rt.tools }
func (rt *RequestTools) AvailableTools() []string { return rt.available }
func (rt *RequestTools) SetContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	rt.mu.Lock()
	rt.ctx = ctx
	rt.mu.Unlock()
}
func (rt *RequestTools) CancelCurrent(err error) {
	rt.mu.Lock()
	batch := rt.batch
	rt.mu.Unlock()
	if batch != nil {
		batch.Cancel(err)
	}
}
func (rt *RequestTools) PermissionHandler() copilot.PermissionHandlerFunc {
	allowed := make(map[string]struct{}, len(rt.permitted))
	for name := range rt.permitted {
		allowed[name] = struct{}{}
	}
	return func(request copilot.PermissionRequest, invocation copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		if request.Kind() == copilot.PermissionRequestKindCustomTool {
			if name, ok := permissionToolName(request); ok {
				if _, allowedTool := allowed[name]; allowedTool {
					return &rpc.PermissionDecisionApproveOnce{}, nil
				}
			}
		}
		return &rpc.PermissionDecisionReject{}, nil
	}
}

func permissionToolName(request copilot.PermissionRequest) (string, bool) {
	switch r := request.(type) {
	case copilot.PermissionRequestCustomTool:
		return r.ToolName, true
	case *copilot.PermissionRequestCustomTool:
		if r == nil {
			return "", false
		}
		return r.ToolName, true
	default:
		return "", false
	}
}

func (rt *RequestTools) CaptureRequests(reqs []copilot.AssistantMessageToolRequest, responseID string, kind string, model string, done <-chan TurnFinalResult, abort func()) (*Batch, []CapturedCall, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.batch == nil || !rt.batch.isOpen() {
		rt.batch = newBatch(rt.broker.ttl, responseID, kind, model, done, abort, rt.ctx)
	} else {
		rt.batch.configure(responseID, kind, model, done, abort)
	}
	calls := make([]CapturedCall, 0, len(reqs))
	for _, req := range reqs {
		args := rawArgs(req.Arguments)
		meta, ok := rt.clientTool(req.Name)
		if !ok {
			return nil, nil, fmt.Errorf("unconfigured SDK tool request %q", req.Name)
		}
		if req.Type != nil && string(*req.Type) == "custom" && meta.ResponseKind == toolcatalog.ToolKindFunction {
			meta.ResponseKind = toolcatalog.ToolKindCustom
		}
		if err := validateStrictArguments(meta, args); err != nil {
			return nil, nil, err
		}
		input := ""
		if meta.ResponseKind == toolcatalog.ToolKindCustom {
			input = customInput(req.Arguments, args)
		}
		call := rt.batch.ensureCall(req.ToolCallID, req.Name, meta, args, input, rt.takeReservedCallIDLocked(req.ToolCallID))
		calls = append(calls, call.Captured())
	}
	rt.broker.Register(rt.batch)
	rt.batch.startTimer()
	return rt.batch, calls, nil
}

func (rt *RequestTools) handleInvocation(inv copilot.ToolInvocation) (copilot.ToolResult, error) {
	args := rawArgs(inv.Arguments)
	rt.mu.Lock()
	if rt.batch == nil || !rt.batch.isOpen() {
		rt.batch = newBatch(rt.broker.ttl, "", "", "", nil, nil, rt.ctx)
	}
	batch := rt.batch
	meta, ok := rt.clientTool(inv.ToolName)
	if !ok {
		rt.mu.Unlock()
		return copilot.ToolResult{}, fmt.Errorf("unconfigured SDK tool invocation %q", inv.ToolName)
	}
	if err := validateStrictArguments(meta, args); err != nil {
		rt.mu.Unlock()
		return copilot.ToolResult{}, err
	}
	input := ""
	if meta.ResponseKind == toolcatalog.ToolKindCustom {
		input = customInput(inv.Arguments, args)
	}
	call := batch.ensureCall(inv.ToolCallID, inv.ToolName, meta, args, input, rt.takeReservedCallIDLocked(inv.ToolCallID))
	rt.broker.Register(batch)
	batch.startTimer()
	rt.mu.Unlock()

	output, err := call.wait(batch.Context())
	if err != nil {
		return copilot.ToolResult{}, err
	}
	return copilot.ToolResult{TextResultForLLM: output, ResultType: "success", SessionLog: "client-provided tool output"}, nil
}

func (rt *RequestTools) clientTool(sdkName string) (ClientTool, bool) {
	if rt.client != nil {
		tool, ok := rt.client[sdkName]
		return tool, ok
	}
	return ClientTool{}, false
}

func customInput(original any, args json.RawMessage) string {
	if s, ok := original.(string); ok {
		trim := strings.TrimSpace(s)
		if !json.Valid([]byte(trim)) {
			return s
		}
	}
	var wrapped struct {
		Input *string `json:"input"`
	}
	if err := json.Unmarshal(args, &wrapped); err == nil && wrapped.Input != nil {
		return *wrapped.Input
	}
	var s string
	if err := json.Unmarshal(args, &s); err == nil {
		return s
	}
	if len(args) == 0 || string(args) == "{}" {
		return ""
	}
	return string(args)
}

type TurnFinalResult struct {
	Value any
	Err   error
}

type Batch struct {
	ID         string
	Kind       string
	Model      string
	ResponseID string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	// calls is keyed by the proxy-minted, client-visible tool-call id.
	calls map[string]*Call
	// bySDKID maps the upstream model's tool-call id back to the same *Call.
	// The SDK announces a tool request (CaptureRequests) and separately invokes
	// the tool handler (handleInvocation) for the same tool call, so this index
	// is what reunites the blocked SDK invocation with the call already
	// published to the client. Model-supplied ids are never used as keys in the
	// process-global broker maps; this index is per-batch and therefore cannot
	// collide across requests.
	bySDKID     map[string]*Call
	Done        <-chan TurnFinalResult
	abort       func()
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	expired     bool
	completed   bool
	timer       *time.Timer
	expireHooks []func(*Batch)
	// broker is the broker that must forget this batch when it closes. It is a
	// single field rather than one expiry hook per Register call, which is what
	// keeps closing an N-tool turn linear in N instead of quadratic.
	broker *Broker
}

func newBatch(ttl time.Duration, responseID string, kind string, model string, done <-chan TurnFinalResult, abort func(), parent context.Context) *Batch {
	now := time.Now().UTC()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	return &Batch{ID: "batch_" + uuid.NewString(), Kind: kind, Model: model, ResponseID: responseID, CreatedAt: now, ExpiresAt: now.Add(ttl), calls: map[string]*Call{}, bySDKID: map[string]*Call{}, Done: done, abort: abort, ctx: ctx, cancel: cancel}
}

func (b *Batch) isOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.expired && !b.completed
}

// callIDs snapshots the batch's call ids under batch.mu. The call map is only
// ever safe to read while that mutex is held, and SDK tool handlers keep adding
// to it until the batch closes, so every caller outside this file's locked
// sections must iterate a snapshot instead of the live map.
//
// Callers must not already hold Broker.mu: findByCallIDs takes batch.mu while
// holding the broker mutex, so the broker mutex always precedes batch.mu.
func (b *Batch) callIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([]string, 0, len(b.calls))
	for id := range b.calls {
		ids = append(ids, id)
	}
	return ids
}

func (b *Batch) Context() context.Context {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctx == nil {
		return context.Background()
	}
	return b.ctx
}

// bindBroker records the broker that must forget this batch when it closes.
// Re-binding the same broker is the normal case (every handleInvocation
// registers again) and costs nothing.
//
// It reports false when the batch has already closed: close has run, so nothing
// this call records would ever be acted on, and the caller must clean up
// directly instead.
func (b *Batch) bindBroker(broker *Broker) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.expired {
		return false
	}
	// Rebinding to a *different* broker would silently orphan the first one's
	// batches/byCall entries, since closeBatch only removes from the broker the
	// field currently holds. Nothing can reach that today - RequestTools.broker
	// is immutable and a gateway owns exactly one Broker - but "removed exactly
	// once" was previously described as a property of this field when it is
	// really a property of the caller, so make the field enforce it rather than
	// inherit it.
	if b.broker != nil && b.broker != broker {
		return false
	}
	b.broker = broker
	return true
}

// OnExpire registers an additional close callback. Broker removal does not go
// through here; see Batch.broker.
func (b *Batch) OnExpire(hook func(*Batch)) {
	if hook == nil {
		return
	}
	b.mu.Lock()
	if b.expired {
		b.mu.Unlock()
		hook(b)
		return
	}
	b.expireHooks = append(b.expireHooks, hook)
	b.mu.Unlock()
}

func (b *Batch) configure(responseID, kind string, model string, done <-chan TurnFinalResult, abort func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if responseID != "" {
		b.ResponseID = responseID
	}
	if kind != "" {
		b.Kind = kind
	}
	if model != "" {
		b.Model = model
	}
	if done != nil {
		b.Done = done
	}
	if abort != nil {
		b.abort = abort
	}
}

// ensureCall returns the batch's call for sdkID, creating it on first sight.
//
// The client-visible id is always minted by this proxy, never taken from the
// model: sdkID comes from upstream, and Copilot's backends are not uniform,
// with some emitting low-entropy sequential ids such as "call_1". Publishing
// those verbatim would make them keys in the process-global Broker.byCall map,
// where two concurrent requests can collide and hand one client's continuation
// another client's pending batch. The model's id is retained on Call.SDKID and
// indexed in b.bySDKID, which is per-batch and only used to answer the SDK.
//
// reservedID is the id ReserveStreamingCall already published on the wire for
// this call, and is empty for a call whose arguments were never streamed.
func (b *Batch) ensureCall(sdkID, sdkName string, meta ClientTool, args json.RawMessage, input string, reservedID string) *Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	if meta.SDKName == "" {
		meta.SDKName = sdkName
	}
	if meta.ResponseName == "" {
		meta.ResponseName = sdkName
	}
	if meta.ResponseKind == "" {
		meta.ResponseKind = toolcatalog.ToolKindFunction
	}
	// An empty sdkID cannot be correlated back to a specific SDK tool request,
	// so every such request becomes its own call.
	if sdkID != "" {
		if call, ok := b.bySDKID[sdkID]; ok {
			if len(call.ArgumentsJSON) == 0 && len(args) > 0 {
				call.ArgumentsJSON = append(call.ArgumentsJSON[:0], args...)
			}
			if call.Input == "" && input != "" {
				call.Input = input
			}
			return call
		}
	}
	openaiID := reservedID
	if openaiID == "" {
		openaiID = "call_" + uuid.NewString()
	}
	call := &Call{OpenAIID: openaiID, SDKID: sdkID, SDKName: meta.SDKName, PublicName: meta.ResponseName, Namespace: meta.Namespace, Kind: meta.ResponseKind, ArgumentsJSON: append(json.RawMessage{}, args...), Input: input, Execution: meta.Execution, outCh: make(chan string, 1), errCh: make(chan error, 1)}
	if call.Execution == "" && call.Kind == toolcatalog.ToolKindToolSearch {
		call.Execution = "client"
	}
	b.calls[openaiID] = call
	if sdkID != "" {
		b.bySDKID[sdkID] = call
	}
	return call
}

// callBySDKID resolves the upstream model's tool-call id back to the
// proxy-owned call, which carries the client-visible id and the channel the
// blocked SDK tool handler is waiting on.
func (b *Batch) callBySDKID(sdkID string) (*Call, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if sdkID == "" {
		return nil, false
	}
	call, ok := b.bySDKID[sdkID]
	return call, ok
}

func (b *Batch) startTimer() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer != nil {
		return
	}
	d := time.Until(b.ExpiresAt)
	if d <= 0 {
		d = time.Millisecond
	}
	b.timer = time.AfterFunc(d, func() { b.expire() })
}

// closeBatch terminates an open batch exactly once: it fails any outstanding
// calls with err, cancels the batch context so waiting tool handlers unblock,
// optionally invokes the SDK abort callback, and runs expiry hooks. TTL expiry
// and explicit cancellation share this path so the two stay in lockstep.
func (b *Batch) closeBatch(err error, runAbort bool) {
	b.mu.Lock()
	if b.expired || b.completed {
		b.mu.Unlock()
		return
	}
	b.expired = true
	if b.timer != nil {
		b.timer.Stop()
	}
	calls := make([]*Call, 0, len(b.calls))
	for _, call := range b.calls {
		calls = append(calls, call)
	}
	abort := b.abort
	cancel := b.cancel
	broker := b.broker
	hooks := append([]func(*Batch){}, b.expireHooks...)
	b.mu.Unlock()
	for _, call := range calls {
		call.fail(err)
	}
	if cancel != nil {
		cancel()
	}
	if runAbort && abort != nil {
		abort()
	}
	if broker != nil {
		broker.Remove(b)
	}
	for _, hook := range hooks {
		hook(b)
	}
}

func (b *Batch) expire() { b.closeBatch(ErrExpired, true) }

// Cancel closes the batch in response to request cancellation. It does not run
// the SDK abort callback, which the caller (the turn runner) drives separately.
func (b *Batch) Cancel(err error) {
	if err == nil {
		err = context.Canceled
	}
	b.closeBatch(err, false)
}

func (b *Batch) Complete(outputs map[string]string) error {
	return b.CompleteWithSetup(outputs, nil)
}

func (b *Batch) CompleteWithSetup(outputs map[string]string, setup func()) error {
	wrapped := make(map[string]toolcatalog.ResponseToolOutput, len(outputs))
	for id, output := range outputs {
		wrapped[id] = toolcatalog.ResponseToolOutput{Kind: toolcatalog.ToolKindFunction, CallID: id, Output: output}
	}
	return b.CompleteToolOutputsWithSetup(wrapped, setup)
}

func (b *Batch) CompleteToolOutputsWithSetup(outputs map[string]toolcatalog.ResponseToolOutput, setup func()) error {
	b.mu.Lock()
	if b.expired {
		b.mu.Unlock()
		return ErrExpired
	}
	if time.Now().After(b.ExpiresAt) {
		b.mu.Unlock()
		// Use the common close path so calls, contexts, abort callbacks, and
		// broker/runner expiry hooks are all released even when completion wins
		// the race with the timer callback.
		b.closeBatch(ErrExpired, true)
		return ErrExpired
	}
	if b.completed {
		b.mu.Unlock()
		return fmt.Errorf("pending tool-call batch is already completed")
	}
	if len(outputs) != len(b.calls) {
		b.mu.Unlock()
		return fmt.Errorf("expected exactly one output for each of %d pending tool calls", len(b.calls))
	}
	calls := make([]*Call, 0, len(b.calls))
	for id, output := range outputs {
		call := b.calls[id]
		if call == nil {
			b.mu.Unlock()
			return fmt.Errorf("unknown tool_call_id %q", id)
		}
		if output.Kind != "" && call.Kind != "" && output.Kind != call.Kind {
			b.mu.Unlock()
			return fmt.Errorf("%s output does not match pending %s call %q", output.Kind, call.Kind, id)
		}
		if call.Kind == toolcatalog.ToolKindCustom && output.Name != "" && call.PublicName != "" && output.Name != call.PublicName {
			b.mu.Unlock()
			return fmt.Errorf("custom_tool_call_output name %q does not match pending custom tool %q for call %q", output.Name, call.PublicName, id)
		}
		call.output = output.Output
		calls = append(calls, call)
	}
	b.completed = true
	if b.timer != nil {
		b.timer.Stop()
	}
	b.mu.Unlock()
	if setup != nil {
		setup()
	}
	for _, call := range calls {
		call.deliver(call.output)
	}
	return nil
}

func (b *Batch) ToolCalls() []openai.ChatToolCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]openai.ChatToolCall, 0, len(b.calls))
	for _, call := range b.calls {
		out = append(out, call.ChatToolCall())
	}
	return out
}

func (b *Batch) CapturedCalls() []CapturedCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]CapturedCall, 0, len(b.calls))
	for _, call := range b.calls {
		out = append(out, call.Captured())
	}
	return out
}

func (b *Batch) CapturedCall(callID string) (CapturedCall, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	call := b.calls[callID]
	if call == nil {
		return CapturedCall{}, false
	}
	return call.Captured(), true
}

type Call struct {
	OpenAIID      string
	SDKID         string
	SDKName       string
	PublicName    string
	Namespace     string
	Kind          toolcatalog.ResponsesToolKind
	ArgumentsJSON json.RawMessage
	Input         string
	Execution     string
	output        string
	outCh         chan string
	errCh         chan error
	once          sync.Once
}

func (c *Call) ChatToolCall() openai.ChatToolCall {
	name := c.PublicName
	if name == "" {
		name = c.SDKName
	}
	return openai.ChatToolCall{ID: c.OpenAIID, Type: "function", Function: openai.ToolCallFunction{Name: name, Arguments: string(c.ArgumentsJSON)}}
}

func (c *Call) Captured() CapturedCall {
	kind := c.Kind
	if kind == "" {
		kind = toolcatalog.ToolKindFunction
	}
	args := append(json.RawMessage{}, c.ArgumentsJSON...)
	return CapturedCall{Kind: kind, SDKName: c.SDKName, ResponseName: c.PublicName, Namespace: c.Namespace, CallID: c.OpenAIID, ArgumentsJSON: args, Input: c.Input, Execution: c.Execution}
}

func (c *Call) wait(ctx context.Context) (string, error) {
	select {
	case out := <-c.outCh:
		return out, nil
	case err := <-c.errCh:
		return "", err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (c *Call) deliver(out string) { c.once.Do(func() { c.outCh <- out }) }
func (c *Call) fail(err error)     { c.once.Do(func() { c.errCh <- err }) }

func rawArgs(v any) json.RawMessage {
	if v == nil {
		return json.RawMessage(`{}`)
	}
	if s, ok := v.(string); ok {
		trim := strings.TrimSpace(s)
		if json.Valid([]byte(trim)) {
			return json.RawMessage(trim)
		}
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 {
		return json.RawMessage(`{}`)
	}
	return b
}
