package toolproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/evanlouie/copilot-api/internal/openai"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
	"github.com/google/uuid"
)

const NoToolsSentinel = "__copilot_api_no_tools__"

var (
	ErrExpired  = errors.New("pending tool call batch expired")
	ErrNotFound = errors.New("pending tool call batch not found")
)

type Broker struct {
	mu      sync.Mutex
	batches map[string]*Batch
	byCall  map[string]*Batch
	ttl     time.Duration
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

type RequestTools struct {
	broker        *Broker
	aliasToPublic map[string]string
	publicToAlias map[string]string
	tools         []copilot.Tool
	available     []string
	choiceNone    bool
	mu            sync.Mutex
	ctx           context.Context
	batch         *Batch
}

func NewRequestTools(broker *Broker, tools []openai.Tool, choiceNone bool) (*RequestTools, error) {
	tools = openai.SupportedTools(tools)
	rt := &RequestTools{broker: broker, aliasToPublic: map[string]string{}, publicToAlias: map[string]string{}, choiceNone: choiceNone, ctx: context.Background()}
	if choiceNone || len(tools) == 0 {
		rt.available = []string{NoToolsSentinel}
		return rt, nil
	}
	for _, t := range tools {
		public := t.Function.Name
		alias := makeAlias(public)
		rt.aliasToPublic[alias] = public
		rt.publicToAlias[public] = alias
		params := map[string]any{}
		if len(t.Function.Parameters) > 0 {
			if err := json.Unmarshal(t.Function.Parameters, &params); err != nil {
				return nil, err
			}
		} else {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		aliasCopy := alias
		rt.tools = append(rt.tools, copilot.Tool{
			Name:        alias,
			Description: t.Function.Description,
			Parameters:  params,
			Handler: func(inv copilot.ToolInvocation) (copilot.ToolResult, error) {
				inv.ToolName = aliasCopy
				return rt.handleInvocation(inv)
			},
		})
		rt.available = append(rt.available, alias)
	}
	return rt, nil
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
func (rt *RequestTools) context() context.Context {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.ctx == nil {
		return context.Background()
	}
	return rt.ctx
}
func (rt *RequestTools) PublicName(alias string) string {
	if p := rt.aliasToPublic[alias]; p != "" {
		return p
	}
	return alias
}
func (rt *RequestTools) AliasFor(public string) string { return rt.publicToAlias[public] }

func (rt *RequestTools) PermissionHandler() copilot.PermissionHandlerFunc {
	allowed := map[string]struct{}{}
	for _, name := range rt.available {
		allowed[name] = struct{}{}
	}
	return func(request copilot.PermissionRequest, invocation copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		if request.Kind() == copilot.PermissionRequestKindCustomTool {
			if name, ok := permissionToolName(request); ok {
				if _, allowedTool := allowed[name]; allowedTool && name != NoToolsSentinel {
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

func (rt *RequestTools) CaptureRequests(reqs []copilot.AssistantMessageToolRequest, responseID string, kind string, model string, done <-chan TurnFinalResult, abort func()) (*Batch, []openai.ChatToolCall) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.batch == nil || !rt.batch.isOpen() {
		rt.batch = newBatch(rt.broker.ttl, responseID, kind, model, done, abort, rt.ctx)
	} else {
		rt.batch.configure(responseID, kind, model, done, abort)
	}
	calls := make([]openai.ChatToolCall, 0, len(reqs))
	for _, req := range reqs {
		public := rt.PublicName(req.Name)
		args := rawArgs(req.Arguments)
		call := rt.batch.ensureCall(req.ToolCallID, public, req.Name, args)
		calls = append(calls, openai.ChatToolCall{ID: call.OpenAIID, Type: "function", Function: openai.ToolCallFunction{Name: public, Arguments: string(args)}})
	}
	rt.broker.Register(rt.batch)
	rt.batch.startTimer()
	return rt.batch, calls
}

func (rt *RequestTools) handleInvocation(inv copilot.ToolInvocation) (copilot.ToolResult, error) {
	public := rt.PublicName(inv.ToolName)
	args := rawArgs(inv.Arguments)
	rt.mu.Lock()
	if rt.batch == nil || !rt.batch.isOpen() {
		rt.batch = newBatch(rt.broker.ttl, "", "", "", nil, nil, rt.ctx)
	}
	batch := rt.batch
	call := batch.ensureCall(inv.ToolCallID, public, inv.ToolName, args)
	rt.broker.Register(batch)
	batch.startTimer()
	rt.mu.Unlock()

	output, err := call.wait(batch.Context())
	if err != nil {
		return copilot.ToolResult{}, err
	}
	return copilot.ToolResult{TextResultForLLM: output, ResultType: "success", SessionLog: "client-provided tool output"}, nil
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

func (b *Batch) ensureCall(sdkID, public, alias string, args json.RawMessage) *Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	openaiID := sdkID
	if openaiID == "" {
		openaiID = "call_" + uuid.NewString()
	}
	if call, ok := b.Calls[openaiID]; ok {
		if len(call.ArgumentsJSON) == 0 && len(args) > 0 {
			call.ArgumentsJSON = append(call.ArgumentsJSON[:0], args...)
		}
		return call
	}
	call := &Call{OpenAIID: openaiID, SDKID: sdkID, PublicName: public, AliasName: alias, ArgumentsJSON: append(json.RawMessage{}, args...), outCh: make(chan string, 1), errCh: make(chan error, 1)}
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
	b.mu.Lock()
	if b.expired || time.Now().After(b.ExpiresAt) {
		b.expired = true
		b.mu.Unlock()
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
		call.output = output
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
		out = append(out, openai.ChatToolCall{ID: call.OpenAIID, Type: "function", Function: openai.ToolCallFunction{Name: call.PublicName, Arguments: string(call.ArgumentsJSON)}})
	}
	return out
}

type Call struct {
	OpenAIID      string
	SDKID         string
	PublicName    string
	AliasName     string
	ArgumentsJSON json.RawMessage
	output        string
	outCh         chan string
	errCh         chan error
	once          sync.Once
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

func makeAlias(public string) string {
	base := strings.Builder{}
	for _, r := range public {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			base.WriteRune(r)
		case r == '-':
			base.WriteByte('_')
		}
	}
	if base.Len() == 0 {
		base.WriteString("tool")
	}
	name := "capi_" + strings.Trim(base.String(), "_")
	if len(name) > 50 {
		name = name[:50]
	}
	return name + "_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
}
