package copilotgw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/hydration"
	"github.com/evanlouie/copilot-api/internal/openai"
	"github.com/evanlouie/copilot-api/internal/sessionfs"
	"github.com/evanlouie/copilot-api/internal/sessionstore"
	"github.com/evanlouie/copilot-api/internal/toolproxy"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
	"github.com/google/uuid"
)

type RealGateway struct {
	cfg    config.Config
	log    *slog.Logger
	client *copilot.Client
	fs     *sessionfs.Manager
	store  *sessionstore.Store
	broker *toolproxy.Broker

	modelsMu       sync.Mutex
	models         []Model
	modelsFetched  time.Time
	modelsCacheTTL time.Duration
	pendingMu      sync.Mutex
	pendingRunners map[string]*turnRunner
}

func NewReal(cfg config.Config, store *sessionstore.Store, log *slog.Logger) *RealGateway {
	fs := sessionfs.NewManager(cfg.DataDir)
	opts := &copilot.ClientOptions{
		CLIPath:     cfg.CLIPath,
		Cwd:         cfg.StateDir,
		LogLevel:    "error",
		GitHubToken: cfg.GitHubToken,
		SessionFs: &copilot.SessionFsConfig{
			InitialCwd:       "/",
			SessionStatePath: sessionfs.SessionStatePath,
			Conventions:      rpc.SessionFSSetProviderConventionsPosix,
		},
	}
	return &RealGateway{cfg: cfg, log: log, client: copilot.NewClient(opts), fs: fs, store: store, broker: toolproxy.NewBroker(cfg.ToolCallTTL), modelsCacheTTL: cfg.ModelsCacheTTL, pendingRunners: map[string]*turnRunner{}}
}

func (g *RealGateway) Start(ctx context.Context) error {
	if err := g.store.Ensure(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(g.cfg.DataDir, "sessions"), 0o700); err != nil {
		return err
	}
	if err := g.client.Start(ctx); err != nil {
		return err
	}
	_, err := g.ListModels(ctx)
	return err
}

func (g *RealGateway) Stop() error { return g.client.Stop() }

func (g *RealGateway) Ready(ctx context.Context) error {
	if g.client.State() != copilot.StateConnected {
		return fmt.Errorf("copilot client is %s", g.client.State())
	}
	_, err := g.ListModels(ctx)
	return err
}

func (g *RealGateway) ListModels(ctx context.Context) ([]Model, error) {
	return g.refreshModels(ctx, false)
}

func (g *RealGateway) ValidateModel(ctx context.Context, model string) error {
	_, err := g.findModel(ctx, model)
	return err
}

func (g *RealGateway) refreshModels(ctx context.Context, force bool) ([]Model, error) {
	g.modelsMu.Lock()
	defer g.modelsMu.Unlock()
	if !force && g.models != nil && (g.modelsCacheTTL <= 0 || time.Since(g.modelsFetched) < g.modelsCacheTTL) {
		out := append([]Model(nil), g.models...)
		return out, nil
	}
	var out []Model
	if g.client.RPC != nil {
		list, err := g.client.RPC.Models.List(ctx, &rpc.ModelsListRequest{})
		if err != nil {
			return nil, err
		}
		for _, m := range list.Models {
			supportsVision, visionKnown := rpcVisionSupport(m.Capabilities.Supports)
			vision := rpcVisionLimits(m.Capabilities.Limits)
			meta := modelMetadata(m.Name, supportsVision, visionKnown, vision)
			if len(m.SupportedReasoningEfforts) > 0 {
				meta["supported_reasoning_efforts"] = m.SupportedReasoningEfforts
			}
			if m.DefaultReasoningEffort != nil {
				meta["default_reasoning_effort"] = *m.DefaultReasoningEffort
			}
			out = append(out, Model{ID: m.ID, Name: m.Name, Metadata: meta, VisionKnown: visionKnown, SupportsVision: supportsVision, Vision: vision})
		}
	} else {
		models, err := g.client.ListModels(ctx)
		if err != nil {
			return nil, err
		}
		for _, m := range models {
			vision := sdkVisionLimits(m.Capabilities.Limits.Vision)
			out = append(out, Model{ID: m.ID, Name: m.Name, Metadata: modelMetadata(m.Name, m.Capabilities.Supports.Vision, true, vision), VisionKnown: true, SupportsVision: m.Capabilities.Supports.Vision, Vision: vision})
		}
	}
	g.models = append([]Model(nil), out...)
	g.modelsFetched = time.Now()
	return out, nil
}

func rpcVisionSupport(supports *rpc.ModelCapabilitiesSupports) (bool, bool) {
	if supports == nil || supports.Vision == nil {
		return false, false
	}
	return *supports.Vision, true
}

func rpcVisionLimits(limits *rpc.ModelCapabilitiesLimits) *VisionLimits {
	if limits == nil || limits.Vision == nil {
		return nil
	}
	return &VisionLimits{
		SupportedMediaTypes: limits.Vision.SupportedMediaTypes,
		MaxPromptImages:     limits.Vision.MaxPromptImages,
		MaxPromptImageSize:  limits.Vision.MaxPromptImageSize,
	}
}

func sdkVisionLimits(limits *copilot.ModelVisionLimits) *VisionLimits {
	if limits == nil {
		return nil
	}
	return &VisionLimits{
		SupportedMediaTypes: limits.SupportedMediaTypes,
		MaxPromptImages:     int64(limits.MaxPromptImages),
		MaxPromptImageSize:  int64(limits.MaxPromptImageSize),
	}
}

func modelMetadata(name string, supportsVision bool, visionKnown bool, vision *VisionLimits) map[string]any {
	meta := map[string]any{"name": name}
	if visionKnown {
		meta["supports_vision"] = supportsVision
		meta["capabilities"] = map[string]any{
			"supports": map[string]any{"vision": supportsVision},
		}
	}
	if vision != nil {
		meta["vision"] = map[string]any{
			"supported_media_types": vision.SupportedMediaTypes,
			"max_prompt_images":     vision.MaxPromptImages,
			"max_prompt_image_size": vision.MaxPromptImageSize,
		}
	}
	return meta
}

func (g *RealGateway) Chat(ctx context.Context, req ChatRequest) (*TurnResult, error) {
	if err := g.ValidateModel(ctx, req.Model); err != nil {
		return nil, err
	}
	finalPrompt, err := req.FinalUser.Prompt()
	if err != nil {
		return nil, openai.InvalidRequest(err.Error(), "messages")
	}
	final, err := g.resolvePrompt(ctx, req.Model, finalPrompt, "messages")
	if err != nil {
		return nil, err
	}
	history, err := g.resolveChatHistory(ctx, req.Model, req.History)
	if err != nil {
		return nil, err
	}
	sessionID := "chat_" + uuid.NewString()
	h, err := hydration.BuildChatHistoryMessages(history, hydration.Options{SessionID: sessionID, Model: req.Model})
	if err != nil {
		return nil, openai.InvalidRequest("failed to hydrate chat history: "+err.Error(), "messages")
	}
	retained, err := sessionfs.WriteEvents(g.cfg.DataDir, sessionID, h.JSONL)
	if err != nil {
		return nil, openai.Internal("failed to write synthetic session state: " + err.Error())
	}
	rt, err := toolproxy.NewRequestTools(g.broker, req.Tools, req.ToolChoiceNone)
	if err != nil {
		return nil, openai.InvalidRequest(err.Error(), "tools")
	}
	events := make(chan copilot.SessionEvent, 256)
	session, err := g.resumeSession(ctx, sessionID, req.Model, req.Instructions, req.ReasoningEffort, rt, false, events)
	if err != nil {
		return nil, openai.Upstream(err.Error())
	}
	runner := g.newTurnRunner(req.OpenAIID, req.Model, session, rt, events, retained, "chat", "")
	if _, err := session.Send(ctx, copilot.MessageOptions{Prompt: final.Text, Attachments: final.Attachments}); err != nil {
		_ = session.Disconnect()
		return nil, openai.Upstream(err.Error())
	}
	result, err := runner.waitInitial(ctx)
	if err != nil {
		return nil, err
	}
	if result.PendingBatchID != "" {
		g.rememberRunner(result.PendingBatchID, runner)
	}
	_ = g.store.SaveSessionMetadata(sessionID, sessionstore.SessionMetadata{ID: sessionID, Kind: "chat", OpenAIID: result.ID, SDKSessionID: sessionID, Model: req.Model, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), RetainedPath: retained, FinishReason: result.FinishReason, PendingBatchID: result.PendingBatchID})
	return result, nil
}

func (g *RealGateway) ContinueChatToolCalls(ctx context.Context, model string, outputs map[string]string) (*TurnResult, error) {
	ids := make([]string, 0, len(outputs))
	for id := range outputs {
		ids = append(ids, id)
	}
	batch, err := g.broker.FindByCallIDs(ids)
	if err != nil {
		return nil, openai.InvalidRequest("unknown or expired tool_call_id", "messages")
	}
	if batch.Kind != "chat" {
		return nil, openai.InvalidRequest("tool_call_id does not belong to a Chat Completions pending batch", "messages")
	}
	runner := g.runnerForBatch(batch.ID)
	if err := batch.Complete(outputs); err != nil {
		return nil, openai.InvalidRequest(err.Error(), "messages")
	}
	g.broker.Remove(batch)
	g.forgetRunner(batch.ID)
	select {
	case final := <-batch.Done:
		if final.Err != nil {
			return nil, final.Err
		}
		turn, ok := final.Value.(*TurnResult)
		if !ok {
			return nil, openai.Internal("unexpected continuation result")
		}
		if turn.PendingBatchID != "" && runner != nil {
			g.rememberRunner(turn.PendingBatchID, runner)
		}
		return turn, nil
	case <-ctx.Done():
		return nil, openai.InvalidRequest(ctx.Err().Error(), "messages")
	}
}

func (g *RealGateway) StreamContinueChatToolCalls(ctx context.Context, model string, outputs map[string]string) (<-chan StreamEvent, error) {
	ids := make([]string, 0, len(outputs))
	for id := range outputs {
		ids = append(ids, id)
	}
	batch, err := g.broker.FindByCallIDs(ids)
	if err != nil {
		return nil, openai.InvalidRequest("unknown or expired tool_call_id", "messages")
	}
	if batch.Kind != "chat" {
		return nil, openai.InvalidRequest("tool_call_id does not belong to a Chat Completions pending batch", "messages")
	}
	runner := g.runnerForBatch(batch.ID)
	if runner == nil {
		return nil, openai.InvalidRequest("pending tool_call_id is not attached to a live streamable turn", "messages")
	}
	ch := make(chan StreamEvent, 32)
	runner.enableChatStream(ch)
	runner.onResult = func(result *TurnResult) {
		if result.PendingBatchID != "" {
			g.rememberRunner(result.PendingBatchID, runner)
		}
		_ = g.store.SaveSessionMetadata(runner.session.SessionID, sessionstore.SessionMetadata{ID: runner.session.SessionID, Kind: "chat", OpenAIID: result.ID, SDKSessionID: runner.session.SessionID, Model: runner.model, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), RetainedPath: runner.retained, FinishReason: result.FinishReason, PendingBatchID: result.PendingBatchID})
	}
	if err := batch.Complete(outputs); err != nil {
		runner.enableChatStream(nil)
		close(ch)
		return nil, openai.InvalidRequest(err.Error(), "messages")
	}
	g.broker.Remove(batch)
	g.forgetRunner(batch.ID)
	go runner.discardInitial()
	return ch, nil
}

func (g *RealGateway) rememberRunner(batchID string, runner *turnRunner) {
	if batchID == "" || runner == nil {
		return
	}
	g.pendingMu.Lock()
	defer g.pendingMu.Unlock()
	g.pendingRunners[batchID] = runner
}

func (g *RealGateway) runnerForBatch(batchID string) *turnRunner {
	g.pendingMu.Lock()
	defer g.pendingMu.Unlock()
	return g.pendingRunners[batchID]
}

func (g *RealGateway) forgetRunner(batchID string) {
	g.pendingMu.Lock()
	defer g.pendingMu.Unlock()
	delete(g.pendingRunners, batchID)
}

func (g *RealGateway) StreamChat(ctx context.Context, req ChatRequest) (<-chan StreamEvent, error) {
	if err := g.ValidateModel(ctx, req.Model); err != nil {
		return nil, err
	}
	finalPrompt, err := req.FinalUser.Prompt()
	if err != nil {
		return nil, openai.InvalidRequest(err.Error(), "messages")
	}
	final, err := g.resolvePrompt(ctx, req.Model, finalPrompt, "messages")
	if err != nil {
		return nil, err
	}
	history, err := g.resolveChatHistory(ctx, req.Model, req.History)
	if err != nil {
		return nil, err
	}
	sessionID := "chat_" + uuid.NewString()
	h, err := hydration.BuildChatHistoryMessages(history, hydration.Options{SessionID: sessionID, Model: req.Model})
	if err != nil {
		return nil, openai.InvalidRequest("failed to hydrate chat history: "+err.Error(), "messages")
	}
	retained, err := sessionfs.WriteEvents(g.cfg.DataDir, sessionID, h.JSONL)
	if err != nil {
		return nil, openai.Internal("failed to write synthetic session state: " + err.Error())
	}
	rt, err := toolproxy.NewRequestTools(g.broker, req.Tools, req.ToolChoiceNone)
	if err != nil {
		return nil, openai.InvalidRequest(err.Error(), "tools")
	}
	events := make(chan copilot.SessionEvent, 256)
	session, err := g.resumeSession(ctx, sessionID, req.Model, req.Instructions, req.ReasoningEffort, rt, true, events)
	if err != nil {
		return nil, openai.Upstream(err.Error())
	}
	ch := make(chan StreamEvent, 32)
	runner := g.newTurnRunner(req.OpenAIID, req.Model, session, rt, events, retained, "chat", "")
	runner.enableChatStream(ch)
	runner.onResult = func(result *TurnResult) {
		if result.PendingBatchID != "" {
			g.rememberRunner(result.PendingBatchID, runner)
		}
		_ = g.store.SaveSessionMetadata(sessionID, sessionstore.SessionMetadata{ID: sessionID, Kind: "chat", OpenAIID: result.ID, SDKSessionID: sessionID, Model: req.Model, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), RetainedPath: retained, FinishReason: result.FinishReason, PendingBatchID: result.PendingBatchID})
	}
	if _, err := session.Send(ctx, copilot.MessageOptions{Prompt: final.Text, Attachments: final.Attachments}); err != nil {
		_ = session.Disconnect()
		return nil, openai.Upstream(err.Error())
	}
	go runner.discardInitial()
	return ch, nil
}

func (g *RealGateway) CreateResponse(ctx context.Context, req ResponseRequest) (*ResponseResult, error) {
	if err := g.ValidateModel(ctx, req.Model); err != nil {
		return nil, err
	}
	if req.ResponseID == "" {
		req.ResponseID = openai.NewID("resp_")
	}
	storeVisible := req.Store
	if !storeVisible {
		// OpenAI Responses defaults store to true. The http layer sets this explicitly.
		storeVisible = false
	}

	if len(req.FunctionOutputs) > 0 {
		return g.continueToolResponse(ctx, req)
	}
	prompt, err := g.resolvePrompt(ctx, req.Model, req.Input, "input")
	if err != nil {
		return nil, err
	}

	rt, err := toolproxy.NewRequestTools(g.broker, req.Tools, req.ToolChoiceNone)
	if err != nil {
		return nil, openai.InvalidRequest(err.Error(), "tools")
	}
	events := make(chan copilot.SessionEvent, 256)
	var session *copilot.Session
	var sessionID string
	var previous *string
	if req.PreviousResponseID != "" {
		record, err := g.store.LoadResponseForContinuation(req.PreviousResponseID)
		if err != nil {
			return nil, openai.NotFound("previous_response_id not found", "not_found")
		}
		if !record.Stored {
			return nil, openai.NotFound("previous_response_id not found", "not_found")
		}
		sessionID = record.SDKSessionID
		previous = &req.PreviousResponseID
		session, err = g.resumeSession(ctx, sessionID, req.Model, req.Instructions, req.ReasoningEffort, rt, false, events)
	} else {
		sessionID = "resp_sdk_" + uuid.NewString()
		session, err = g.createSession(ctx, sessionID, req.Model, req.Instructions, req.ReasoningEffort, rt, false, events)
	}
	if err != nil {
		return nil, openai.Upstream(err.Error())
	}
	retained := g.fs.SessionRoot(sessionID)
	runner := g.newTurnRunner(req.ResponseID, req.Model, session, rt, events, retained, "response", req.ResponseID)
	if _, err := session.Send(ctx, copilot.MessageOptions{Prompt: prompt.Text, Attachments: prompt.Attachments}); err != nil {
		_ = session.Disconnect()
		return nil, openai.Upstream(err.Error())
	}
	turn, err := runner.waitInitial(ctx)
	if err != nil {
		return nil, err
	}
	if turn.PendingBatchID != "" {
		g.rememberRunner(turn.PendingBatchID, runner)
	}
	resp := responseFromTurn(req.ResponseID, req.Model, req.Instructions, previous, storeVisible, turn)
	record := recordFromResponse(resp, sessionID, retained)
	record.PendingBatchID = turn.PendingBatchID
	if err := g.store.SaveResponse(record); err != nil {
		return nil, openai.Internal(err.Error())
	}
	return &ResponseResult{Response: resp}, nil
}

func (g *RealGateway) StreamResponse(ctx context.Context, req ResponseRequest) (<-chan ResponseStreamEvent, error) {
	if len(req.FunctionOutputs) > 0 {
		ids := make([]string, 0, len(req.FunctionOutputs))
		for id := range req.FunctionOutputs {
			ids = append(ids, id)
		}
		batch, err := g.broker.FindByCallIDs(ids)
		if err != nil {
			return nil, openai.InvalidRequest("unknown or expired function_call_output call_id", "input")
		}
		if batch.ResponseID != "" && req.PreviousResponseID != batch.ResponseID {
			return nil, openai.InvalidRequest("function_call_output call_id does not belong to previous_response_id", "input")
		}
		previousRecord, err := g.store.LoadResponseForContinuation(req.PreviousResponseID)
		if err != nil {
			return nil, openai.NotFound("previous_response_id not found", "not_found")
		}
		runner := g.runnerForBatch(batch.ID)
		if runner == nil {
			return nil, openai.InvalidRequest("pending function_call_output is not attached to a live streamable turn", "input")
		}
		storeVisible := req.Store
		if !req.StoreSet {
			storeVisible = previousRecord.Stored
		}
		previous := req.PreviousResponseID
		ch := make(chan ResponseStreamEvent, 32)
		runner.enableResponseStream(ch, req.ResponseID, req.Model, req.Instructions, &previous, storeVisible)
		runner.onResult = func(turn *TurnResult) {
			if turn.PendingBatchID != "" {
				g.rememberRunner(turn.PendingBatchID, runner)
			}
			resp := responseFromTurn(req.ResponseID, req.Model, req.Instructions, &previous, storeVisible, turn)
			record := recordFromResponse(resp, turn.SDKSessionID, turn.RetainedPath)
			record.PendingBatchID = turn.PendingBatchID
			_ = g.store.SaveResponse(record)
		}
		if err := batch.Complete(req.FunctionOutputs); err != nil {
			runner.enableResponseStream(nil, "", "", "", nil, false)
			close(ch)
			return nil, openai.InvalidRequest(err.Error(), "input")
		}
		g.broker.Remove(batch)
		g.forgetRunner(batch.ID)
		go runner.discardInitial()
		return ch, nil
	}
	if err := g.ValidateModel(ctx, req.Model); err != nil {
		return nil, err
	}
	if req.ResponseID == "" {
		req.ResponseID = openai.NewID("resp_")
	}
	prompt, err := g.resolvePrompt(ctx, req.Model, req.Input, "input")
	if err != nil {
		return nil, err
	}
	rt, err := toolproxy.NewRequestTools(g.broker, req.Tools, req.ToolChoiceNone)
	if err != nil {
		return nil, openai.InvalidRequest(err.Error(), "tools")
	}
	events := make(chan copilot.SessionEvent, 256)
	var session *copilot.Session
	var sessionID string
	var previous *string
	if req.PreviousResponseID != "" {
		record, err := g.store.LoadResponseForContinuation(req.PreviousResponseID)
		if err != nil || !record.Stored {
			return nil, openai.NotFound("previous_response_id not found", "not_found")
		}
		sessionID = record.SDKSessionID
		previous = &req.PreviousResponseID
		session, err = g.resumeSession(ctx, sessionID, req.Model, req.Instructions, req.ReasoningEffort, rt, true, events)
	} else {
		sessionID = "resp_sdk_" + uuid.NewString()
		session, err = g.createSession(ctx, sessionID, req.Model, req.Instructions, req.ReasoningEffort, rt, true, events)
	}
	if err != nil {
		return nil, openai.Upstream(err.Error())
	}
	retained := g.fs.SessionRoot(sessionID)
	ch := make(chan ResponseStreamEvent, 32)
	runner := g.newTurnRunner(req.ResponseID, req.Model, session, rt, events, retained, "response", req.ResponseID)
	runner.enableResponseStream(ch, req.ResponseID, req.Model, req.Instructions, previous, req.Store)
	runner.onResult = func(turn *TurnResult) {
		if turn.PendingBatchID != "" {
			g.rememberRunner(turn.PendingBatchID, runner)
		}
		resp := responseFromTurn(req.ResponseID, req.Model, req.Instructions, previous, req.Store, turn)
		record := recordFromResponse(resp, sessionID, retained)
		record.PendingBatchID = turn.PendingBatchID
		_ = g.store.SaveResponse(record)
	}
	if _, err := session.Send(ctx, copilot.MessageOptions{Prompt: prompt.Text, Attachments: prompt.Attachments}); err != nil {
		_ = session.Disconnect()
		return nil, openai.Upstream(err.Error())
	}
	go runner.discardInitial()
	return ch, nil
}

func (g *RealGateway) continueToolResponse(ctx context.Context, req ResponseRequest) (*ResponseResult, error) {
	ids := make([]string, 0, len(req.FunctionOutputs))
	for id := range req.FunctionOutputs {
		ids = append(ids, id)
	}
	batch, err := g.broker.FindByCallIDs(ids)
	if err != nil {
		return nil, openai.InvalidRequest("unknown or expired function_call_output call_id", "input")
	}
	if batch.ResponseID != "" && req.PreviousResponseID != batch.ResponseID {
		return nil, openai.InvalidRequest("function_call_output call_id does not belong to previous_response_id", "input")
	}
	previousRecord, err := g.store.LoadResponseForContinuation(req.PreviousResponseID)
	if err != nil {
		return nil, openai.NotFound("previous_response_id not found", "not_found")
	}
	runner := g.runnerForBatch(batch.ID)
	if err := batch.Complete(req.FunctionOutputs); err != nil {
		return nil, openai.InvalidRequest(err.Error(), "input")
	}
	g.broker.Remove(batch)
	g.forgetRunner(batch.ID)
	select {
	case final := <-batch.Done:
		if final.Err != nil {
			return nil, final.Err
		}
		turn, ok := final.Value.(*TurnResult)
		if !ok {
			return nil, openai.Internal("unexpected continuation result")
		}
		if turn.PendingBatchID != "" && runner != nil {
			g.rememberRunner(turn.PendingBatchID, runner)
		}
		previous := req.PreviousResponseID
		storeVisible := req.Store
		if !req.StoreSet {
			storeVisible = previousRecord.Stored
		}
		resp := responseFromTurn(req.ResponseID, req.Model, req.Instructions, &previous, storeVisible, turn)
		record := recordFromResponse(resp, turn.SDKSessionID, turn.RetainedPath)
		record.PendingBatchID = turn.PendingBatchID
		if err := g.store.SaveResponse(record); err != nil {
			return nil, openai.Internal(err.Error())
		}
		return &ResponseResult{Response: resp}, nil
	case <-ctx.Done():
		return nil, openai.InvalidRequest(ctx.Err().Error(), "input")
	}
}

func (g *RealGateway) GetResponse(ctx context.Context, id string) (*openai.Response, error) {
	record, err := g.store.LoadResponse(id)
	if err != nil {
		if errors.Is(err, sessionstore.ErrNotFound) {
			return nil, openai.NotFound("response not found", "not_found")
		}
		return nil, openai.Internal(err.Error())
	}
	resp := &openai.Response{ID: record.ID, Object: openai.ObjectResponse, CreatedAt: record.CreatedAt.Unix(), Status: record.Status, Model: record.Model, Instructions: record.Instructions, Output: record.Output, OutputText: record.OutputText, Store: record.Stored, Usage: record.Usage, Error: nil, IncompleteDetails: nil, ParallelToolCalls: true}
	if record.PreviousResponseID != "" {
		resp.PreviousResponseID = &record.PreviousResponseID
	}
	return resp, nil
}

func (g *RealGateway) DeleteResponse(ctx context.Context, id string) error {
	if err := g.store.DeleteResponse(id); err != nil {
		if errors.Is(err, sessionstore.ErrNotFound) {
			return openai.NotFound("response not found", "not_found")
		}
		return openai.Internal(err.Error())
	}
	return nil
}

func (g *RealGateway) resolveChatHistory(ctx context.Context, model string, messages []openai.ChatMessage) ([]hydration.Message, error) {
	out := make([]hydration.Message, 0, len(messages))
	for i, msg := range messages {
		switch msg.Role {
		case "user":
			prompt, err := msg.Prompt()
			if err != nil {
				return nil, openai.InvalidRequest(err.Error(), fmt.Sprintf("messages.%d.content", i))
			}
			resolved, err := g.resolvePrompt(ctx, model, prompt, fmt.Sprintf("messages.%d.content", i))
			if err != nil {
				return nil, err
			}
			out = append(out, hydration.Message{Role: msg.Role, Content: resolved.Text, Attachments: resolved.Attachments})
		case "assistant", "tool":
			text, err := msg.Text()
			if err != nil {
				return nil, openai.InvalidRequest(err.Error(), fmt.Sprintf("messages.%d.content", i))
			}
			out = append(out, hydration.Message{Role: msg.Role, Content: text, ToolCallID: msg.ToolCallID, ToolCalls: msg.ToolCalls})
		default:
			return nil, openai.InvalidRequest(fmt.Sprintf("unsupported message role %q", msg.Role), fmt.Sprintf("messages.%d.role", i))
		}
	}
	return out, nil
}

func (g *RealGateway) createSession(ctx context.Context, sessionID, model, instructions, reasoning string, rt *toolproxy.RequestTools, streaming bool, events chan<- copilot.SessionEvent) (*copilot.Session, error) {
	var lastErr error
	for _, candidate := range openai.InstructionCandidates(instructions) {
		cfg := &copilot.SessionConfig{
			SessionID:                      sessionID,
			ClientName:                     "copilot-api",
			Model:                          model,
			ReasoningEffort:                reasoning,
			Tools:                          rt.Tools(),
			AvailableTools:                 rt.AvailableTools(),
			SystemMessage:                  &copilot.SystemMessageConfig{Mode: "replace", Content: candidate},
			OnPermissionRequest:            rt.PermissionHandler(),
			WorkingDirectory:               "/",
			ConfigDir:                      g.cfg.ConfigDir,
			EnableConfigDiscovery:          false,
			MCPServers:                     map[string]copilot.MCPServerConfig{},
			SkillDirectories:               nil,
			DisabledSkills:                 []string{"*"},
			InfiniteSessions:               &copilot.InfiniteSessionConfig{Enabled: copilot.Bool(false)},
			Streaming:                      streaming,
			IncludeSubAgentStreamingEvents: copilot.Bool(false),
			OnEvent:                        func(e copilot.SessionEvent) { sendEvent(events, e) },
			CreateSessionFsHandler:         func(session *copilot.Session) copilot.SessionFsProvider { return g.fs.Provider(session.SessionID) },
		}
		if g.cfg.GitHubToken != "" {
			cfg.GitHubToken = g.cfg.GitHubToken
		}
		s, err := g.client.CreateSession(ctx, cfg)
		if err == nil {
			return s, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (g *RealGateway) resumeSession(ctx context.Context, sessionID, model, instructions, reasoning string, rt *toolproxy.RequestTools, streaming bool, events chan<- copilot.SessionEvent) (*copilot.Session, error) {
	var lastErr error
	for _, candidate := range openai.InstructionCandidates(instructions) {
		cfg := &copilot.ResumeSessionConfig{
			ClientName:                     "copilot-api",
			Model:                          model,
			ReasoningEffort:                reasoning,
			Tools:                          rt.Tools(),
			AvailableTools:                 rt.AvailableTools(),
			SystemMessage:                  &copilot.SystemMessageConfig{Mode: "replace", Content: candidate},
			OnPermissionRequest:            rt.PermissionHandler(),
			WorkingDirectory:               "/",
			ConfigDir:                      g.cfg.ConfigDir,
			EnableConfigDiscovery:          false,
			MCPServers:                     map[string]copilot.MCPServerConfig{},
			SkillDirectories:               nil,
			DisabledSkills:                 []string{"*"},
			InfiniteSessions:               &copilot.InfiniteSessionConfig{Enabled: copilot.Bool(false)},
			Streaming:                      streaming,
			IncludeSubAgentStreamingEvents: copilot.Bool(false),
			OnEvent:                        func(e copilot.SessionEvent) { sendEvent(events, e) },
			CreateSessionFsHandler:         func(session *copilot.Session) copilot.SessionFsProvider { return g.fs.Provider(session.SessionID) },
		}
		if g.cfg.GitHubToken != "" {
			cfg.GitHubToken = g.cfg.GitHubToken
		}
		s, err := g.client.ResumeSession(ctx, sessionID, cfg)
		if err == nil {
			return s, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func sendEvent(ch chan<- copilot.SessionEvent, e copilot.SessionEvent) {
	select {
	case ch <- e:
	default:
	}
}
