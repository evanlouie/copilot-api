package toolproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
	"github.com/google/uuid"
)

const NoToolsSentinel = openai.NoToolsSentinelName

var (
	ErrExpired  = errors.New("pending tool call batch expired")
	ErrNotFound = errors.New("pending tool call batch not found")
)

type ClientTool struct {
	SDKName      string
	ResponseKind openai.ResponsesToolKind
	ResponseName string
	Namespace    string
	Description  string
	Parameters   map[string]any
	Strict       *bool
	DeferLoading *bool
	Execution    string
	Raw          json.RawMessage
}

type CapturedCall struct {
	Kind          openai.ResponsesToolKind
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

func (b *Broker) Register(batch *Batch) {
	batch.OnExpire(func(expired *Batch) { b.Remove(expired) })
	if !batch.isOpen() {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		batch.Cancel(context.Canceled)
		return
	}
	defer b.mu.Unlock()
	b.batches[batch.ID] = batch
	for id := range batch.Calls {
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
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.batches, batch.ID)
	for id := range batch.Calls {
		delete(b.byCall, id)
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
}

func NewRequestTools(broker *Broker, tools []openai.Tool, choiceNone bool) (*RequestTools, error) {
	tools = openai.SupportedTools(tools)
	clientTools := make([]ClientTool, 0, len(tools))
	for _, t := range tools {
		params, err := schemaMap(t.Function.Parameters)
		if err != nil {
			return nil, err
		}
		clientTools = append(clientTools, ClientTool{SDKName: t.Function.Name, ResponseKind: openai.ToolKindFunction, ResponseName: t.Function.Name, Description: t.Function.Description, Parameters: params, Strict: t.Function.Strict})
	}
	return newRequestToolsFromClientTools(broker, clientTools, choiceNone)
}

func NewResponseRequestTools(broker *Broker, tools []openai.NormalizedTool, choiceNone bool) (*RequestTools, error) {
	clientTools, err := FlattenResponsesTools(tools)
	if err != nil {
		return nil, err
	}
	return newRequestToolsFromClientTools(broker, clientTools, choiceNone)
}

func newRequestToolsFromClientTools(broker *Broker, clientTools []ClientTool, choiceNone bool) (*RequestTools, error) {
	rt := &RequestTools{broker: broker, permitted: map[string]struct{}{}, client: map[string]ClientTool{}, ctx: context.Background()}
	if choiceNone || len(clientTools) == 0 {
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
		rt.client[ct.SDKName] = ct
		rt.permitted[ct.SDKName] = struct{}{}
		ctCopy := ct
		rt.tools = append(rt.tools, copilot.Tool{
			Name:                 ct.SDKName,
			Description:          ct.Description,
			Parameters:           ct.Parameters,
			OverridesBuiltInTool: true,
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

func FlattenResponsesTools(tools []openai.NormalizedTool) ([]ClientTool, error) {
	flattened := make([]ClientTool, 0, len(tools))
	for _, tool := range tools {
		switch tool.Kind {
		case openai.ToolKindFunction:
			ct, err := clientToolFromNormalized(tool, "")
			if err != nil {
				return nil, err
			}
			flattened = append(flattened, ct)
		case openai.ToolKindCustom:
			ct, err := clientToolFromNormalized(tool, "")
			if err != nil {
				return nil, err
			}
			flattened = append(flattened, ct)
		case openai.ToolKindNamespace:
			for _, child := range tool.Children {
				child.Namespace = tool.Name
				ct, err := clientToolFromNormalized(child, tool.Name)
				if err != nil {
					return nil, err
				}
				flattened = append(flattened, ct)
			}
		case openai.ToolKindToolSearch:
			ct, err := clientToolFromNormalized(tool, "")
			if err != nil {
				return nil, err
			}
			flattened = append(flattened, ct)
		}
	}
	return assignSDKNames(flattened)
}

func clientToolFromNormalized(tool openai.NormalizedTool, namespace string) (ClientTool, error) {
	if namespace == "" {
		namespace = tool.Namespace
	}
	var params map[string]any
	var err error
	switch tool.Kind {
	case openai.ToolKindCustom:
		params = customToolSchema(tool.Name)
	case openai.ToolKindToolSearch:
		params, err = schemaMap(tool.Parameters)
	default:
		params, err = schemaMap(tool.Parameters)
	}
	if err != nil {
		return ClientTool{}, err
	}
	desc := tool.Description
	ct := ClientTool{ResponseKind: tool.Kind, ResponseName: tool.Name, Namespace: namespace, Description: desc, Parameters: params, Strict: tool.Strict, DeferLoading: tool.DeferLoading, Execution: tool.Execution, Raw: tool.Raw}
	if ct.ResponseKind == openai.ToolKindToolSearch && ct.Execution == "" {
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
	return openai.NormalizedToolSDKName(openai.NormalizedTool{Kind: tool.ResponseKind, Name: tool.ResponseName, Namespace: tool.Namespace})
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
	if tool.ResponseKind == openai.ToolKindCustom {
		prefix += " Freeform custom tool; provide the raw tool input in the required JSON string field named input."
	}
	if tool.ResponseKind == openai.ToolKindToolSearch {
		prefix += " Client-executed tool discovery; returns loadable client tool specs."
	}
	if strings.TrimSpace(tool.Description) == "" {
		return prefix
	}
	return prefix + " " + tool.Description
}

func schemaMap(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return map[string]any{"type": "object", "properties": map[string]any{}}, nil
	}
	params := map[string]any{}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, err
	}
	return params, nil
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
		if req.Type != nil && string(*req.Type) == "custom" && meta.ResponseKind == openai.ToolKindFunction {
			meta.ResponseKind = openai.ToolKindCustom
		}
		input := ""
		if meta.ResponseKind == openai.ToolKindCustom {
			input = customInput(req.Arguments, args)
		}
		call := rt.batch.ensureCall(req.ToolCallID, req.Name, meta, args, input)
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
	input := ""
	if meta.ResponseKind == openai.ToolKindCustom {
		input = customInput(inv.Arguments, args)
	}
	call := batch.ensureCall(inv.ToolCallID, inv.ToolName, meta, args, input)
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
	ID          string
	Kind        string
	Model       string
	ResponseID  string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Calls       map[string]*Call
	Done        <-chan TurnFinalResult
	abort       func()
	ctx         context.Context
	cancel      context.CancelFunc
	mu          sync.Mutex
	expired     bool
	completed   bool
	timer       *time.Timer
	expireHooks []func(*Batch)
}

func newBatch(ttl time.Duration, responseID string, kind string, model string, done <-chan TurnFinalResult, abort func(), parent context.Context) *Batch {
	now := time.Now().UTC()
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	return &Batch{ID: "batch_" + uuid.NewString(), Kind: kind, Model: model, ResponseID: responseID, CreatedAt: now, ExpiresAt: now.Add(ttl), Calls: map[string]*Call{}, Done: done, abort: abort, ctx: ctx, cancel: cancel}
}

func (b *Batch) isOpen() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.expired && !b.completed
}

func (b *Batch) Context() context.Context {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ctx == nil {
		return context.Background()
	}
	return b.ctx
}

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

func (b *Batch) ensureCall(sdkID, sdkName string, meta ClientTool, args json.RawMessage, input string) *Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	openaiID := sdkID
	if openaiID == "" {
		openaiID = "call_" + uuid.NewString()
	}
	if meta.SDKName == "" {
		meta.SDKName = sdkName
	}
	if meta.ResponseName == "" {
		meta.ResponseName = sdkName
	}
	if meta.ResponseKind == "" {
		meta.ResponseKind = openai.ToolKindFunction
	}
	if call, ok := b.Calls[openaiID]; ok {
		if len(call.ArgumentsJSON) == 0 && len(args) > 0 {
			call.ArgumentsJSON = append(call.ArgumentsJSON[:0], args...)
		}
		if call.Input == "" && input != "" {
			call.Input = input
		}
		return call
	}
	call := &Call{OpenAIID: openaiID, SDKID: sdkID, SDKName: meta.SDKName, PublicName: meta.ResponseName, Namespace: meta.Namespace, Kind: meta.ResponseKind, ArgumentsJSON: append(json.RawMessage{}, args...), Input: input, Execution: meta.Execution, outCh: make(chan string, 1), errCh: make(chan error, 1)}
	if call.Execution == "" && call.Kind == openai.ToolKindToolSearch {
		call.Execution = "client"
	}
	b.Calls[openaiID] = call
	return call
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
	calls := make([]*Call, 0, len(b.Calls))
	for _, call := range b.Calls {
		calls = append(calls, call)
	}
	abort := b.abort
	cancel := b.cancel
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
	wrapped := make(map[string]openai.ResponseToolOutput, len(outputs))
	for id, output := range outputs {
		wrapped[id] = openai.ResponseToolOutput{Kind: openai.ToolKindFunction, CallID: id, Output: output}
	}
	return b.CompleteToolOutputsWithSetup(wrapped, setup)
}

func (b *Batch) CompleteToolOutputsWithSetup(outputs map[string]openai.ResponseToolOutput, setup func()) error {
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
	if len(outputs) != len(b.Calls) {
		b.mu.Unlock()
		return fmt.Errorf("expected exactly one output for each of %d pending tool calls", len(b.Calls))
	}
	calls := make([]*Call, 0, len(b.Calls))
	for id, output := range outputs {
		call := b.Calls[id]
		if call == nil {
			b.mu.Unlock()
			return fmt.Errorf("unknown tool_call_id %q", id)
		}
		if output.Kind != "" && call.Kind != "" && output.Kind != call.Kind {
			b.mu.Unlock()
			return fmt.Errorf("%s output does not match pending %s call %q", output.Kind, call.Kind, id)
		}
		if call.Kind == openai.ToolKindCustom && output.Name != "" && call.PublicName != "" && output.Name != call.PublicName {
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
	out := make([]openai.ChatToolCall, 0, len(b.Calls))
	for _, call := range b.Calls {
		out = append(out, call.ChatToolCall())
	}
	return out
}

func (b *Batch) CapturedCalls() []CapturedCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]CapturedCall, 0, len(b.Calls))
	for _, call := range b.Calls {
		out = append(out, call.Captured())
	}
	return out
}

func (b *Batch) CapturedCall(callID string) (CapturedCall, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	call := b.Calls[callID]
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
	Kind          openai.ResponsesToolKind
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
		kind = openai.ToolKindFunction
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
