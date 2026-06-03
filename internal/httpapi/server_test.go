package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/observability"
	"github.com/evanlouie/copilot-api/internal/openai"
)

type modelsGateway struct {
	copilotgw.Gateway
	models []copilotgw.Model
}

func (g modelsGateway) ListModels(context.Context) ([]copilotgw.Model, error) {
	return g.models, nil
}

type codexStreamGateway struct {
	copilotgw.Gateway
	got copilotgw.ResponseRequest
}

func (g *codexStreamGateway) StreamResponse(_ context.Context, req copilotgw.ResponseRequest) (<-chan copilotgw.ResponseStreamEvent, error) {
	g.got = req
	ch := make(chan copilotgw.ResponseStreamEvent, 2)
	go func() {
		defer close(ch)
		ch <- copilotgw.ResponseStreamEvent{Kind: "delta", Delta: "ok"}
		ch <- copilotgw.ResponseStreamEvent{Kind: "response", Response: &openai.Response{ID: req.ResponseID, Object: openai.ObjectResponse, CreatedAt: openai.UnixNow(), Status: "completed", Model: req.Model, OutputText: "ok", Output: []openai.ResponseOutputItem{{ID: "msg_final", Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: "ok"}}}}, ParallelToolCalls: true, Store: req.Store}}
	}()
	return ch, nil
}

type captureChatGateway struct {
	copilotgw.Gateway
	got copilotgw.ChatRequest
}

func (g *captureChatGateway) Chat(_ context.Context, req copilotgw.ChatRequest) (*copilotgw.TurnResult, error) {
	g.got = req
	return &copilotgw.TurnResult{ID: req.OpenAIID, Created: openai.UnixNow(), Model: req.Model, Text: "ok", FinishReason: "stop"}, nil
}

type resolvingChatGateway struct {
	copilotgw.Gateway
	got              copilotgw.ChatRequest
	resolveCalls     int
	resolveModel     string
	resolveRequested string
	resolveDefault   string
	resolvedEffort   string
	continueCalled   bool
}

func (g *resolvingChatGateway) ResolveReasoningEffort(_ context.Context, model, requestedEffort, defaultEffort string) (string, error) {
	g.resolveCalls++
	g.resolveModel = model
	g.resolveRequested = requestedEffort
	g.resolveDefault = defaultEffort
	return g.resolvedEffort, nil
}

func (g *resolvingChatGateway) Chat(_ context.Context, req copilotgw.ChatRequest) (*copilotgw.TurnResult, error) {
	g.got = req
	return &copilotgw.TurnResult{ID: req.OpenAIID, Created: openai.UnixNow(), Model: req.Model, Text: "ok", FinishReason: "stop"}, nil
}

func (g *resolvingChatGateway) ContinueChatToolCalls(_ context.Context, model string, outputs map[string]string) (*copilotgw.TurnResult, error) {
	g.continueCalled = true
	return &copilotgw.TurnResult{Model: model, Text: outputs["call_1"], FinishReason: "stop"}, nil
}

type streamChatGateway struct {
	copilotgw.Gateway
	got copilotgw.ChatRequest
}

func (g *streamChatGateway) StreamChat(_ context.Context, req copilotgw.ChatRequest) (<-chan copilotgw.StreamEvent, error) {
	g.got = req
	ch := make(chan copilotgw.StreamEvent, 1)
	go func() {
		defer close(ch)
		prompt, completion, total := int64(3), int64(5), int64(8)
		ch <- copilotgw.StreamEvent{Kind: "result", Result: &copilotgw.TurnResult{
			ID:           req.OpenAIID,
			Created:      openai.UnixNow(),
			Model:        req.Model,
			FinishReason: "tool_calls",
			ToolCalls: []openai.ChatToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: openai.ToolCallFunction{
					Name:      "lookup",
					Arguments: `{"q":"alpha"}`,
				},
			}},
			Usage: &openai.Usage{PromptTokens: &prompt, CompletionTokens: &completion, TotalTokens: &total},
		}}
	}()
	return ch, nil
}

type captureResponseGateway struct {
	copilotgw.Gateway
	got copilotgw.ResponseRequest
}

func (g *captureResponseGateway) CreateResponse(_ context.Context, req copilotgw.ResponseRequest) (*copilotgw.ResponseResult, error) {
	g.got = req
	resp := &openai.Response{ID: req.ResponseID, Object: openai.ObjectResponse, CreatedAt: openai.UnixNow(), Status: "completed", Model: req.Model, OutputText: "ok", Output: []openai.ResponseOutputItem{{ID: "msg_final", Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: "ok"}}}}, ParallelToolCalls: true, Store: req.Store}
	return &copilotgw.ResponseResult{Response: resp}, nil
}

type functionCallStreamGateway struct {
	copilotgw.Gateway
	got copilotgw.ResponseRequest
}

func (g *functionCallStreamGateway) StreamResponse(_ context.Context, req copilotgw.ResponseRequest) (<-chan copilotgw.ResponseStreamEvent, error) {
	g.got = req
	ch := make(chan copilotgw.ResponseStreamEvent, 1)
	go func() {
		defer close(ch)
		ch <- copilotgw.ResponseStreamEvent{Kind: "response", Response: &openai.Response{
			ID:                req.ResponseID,
			Object:            openai.ObjectResponse,
			CreatedAt:         openai.UnixNow(),
			Status:            "completed",
			Model:             req.Model,
			Output:            []openai.ResponseOutputItem{{ID: "fc_call_1", Type: "function_call", Status: "completed", CallID: "call_1", Name: "lookup", Arguments: `{"q":"alpha"}`}},
			ParallelToolCalls: true,
			Store:             req.Store,
		}}
	}()
	return ch, nil
}

type statefulResponseGateway struct {
	copilotgw.Gateway
	resp    *openai.Response
	deleted bool
}

func (g *statefulResponseGateway) GetResponse(_ context.Context, id string) (*openai.Response, error) {
	if g.deleted || g.resp == nil || g.resp.ID != id {
		return nil, openai.NotFound("response not found", "not_found")
	}
	return g.resp, nil
}

func (g *statefulResponseGateway) DeleteResponse(_ context.Context, id string) error {
	if g.deleted || g.resp == nil || g.resp.ID != id {
		return openai.NotFound("response not found", "not_found")
	}
	g.deleted = true
	return nil
}

func TestModelsEndpointIncludesContextWindowLimits(t *testing.T) {
	contextWindow := int64(200000)
	s := New(config.Config{}, modelsGateway{models: []copilotgw.Model{{
		ID: "gpt-5",
		Metadata: map[string]any{
			"max_context_window_tokens": contextWindow,
			"capabilities": map[string]any{
				"limits": map[string]any{
					"max_context_window_tokens": contextWindow,
				},
			},
		},
	}}}, slog.Default())

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var out openai.ModelList
	dec := json.NewDecoder(rr.Body)
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 {
		t.Fatalf("models length = %d, want 1", len(out.Data))
	}
	if got := out.Data[0].Meta["max_context_window_tokens"]; got != json.Number("200000") {
		t.Fatalf("metadata max_context_window_tokens = %#v, want 200000", got)
	}
	capabilities, ok := out.Data[0].Meta["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("metadata capabilities = %#v, want object", out.Data[0].Meta["capabilities"])
	}
	limits, ok := capabilities["limits"].(map[string]any)
	if !ok {
		t.Fatalf("metadata capabilities.limits = %#v, want object", capabilities["limits"])
	}
	if got := limits["max_context_window_tokens"]; got != json.Number("200000") {
		t.Fatalf("metadata capabilities.limits.max_context_window_tokens = %#v, want 200000", got)
	}
}

func TestModelsEndpointIncludesReasoningAndVisionMetadata(t *testing.T) {
	s := New(config.Config{}, modelsGateway{models: []copilotgw.Model{{
		ID: "gpt-5-vision",
		Metadata: map[string]any{
			"supported_reasoning_efforts": []string{"low", "medium", "high"},
			"default_reasoning_effort":    "medium",
			"supports_vision":             true,
			"capabilities": map[string]any{
				"supports": map[string]any{"vision": true},
			},
			"vision": map[string]any{
				"supported_media_types": []string{"image/png", "image/jpeg"},
				"max_prompt_images":     int64(4),
				"max_prompt_image_size": int64(20_000_000),
			},
		},
	}}}, slog.Default())

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var out openai.ModelList
	dec := json.NewDecoder(rr.Body)
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	meta := out.Data[0].Meta
	if got := meta["default_reasoning_effort"]; got != "medium" {
		t.Fatalf("default_reasoning_effort = %#v, want medium", got)
	}
	efforts, ok := meta["supported_reasoning_efforts"].([]any)
	if !ok || len(efforts) != 3 || efforts[2] != "high" {
		t.Fatalf("supported_reasoning_efforts = %#v, want [low medium high]", meta["supported_reasoning_efforts"])
	}
	if got := meta["supports_vision"]; got != true {
		t.Fatalf("supports_vision = %#v, want true", got)
	}
	vision, ok := meta["vision"].(map[string]any)
	if !ok {
		t.Fatalf("vision metadata = %#v, want object", meta["vision"])
	}
	if got := vision["max_prompt_images"]; got != json.Number("4") {
		t.Fatalf("vision.max_prompt_images = %#v, want 4", got)
	}
}

func TestRequestLoggingMiddlewareLogsMetadata(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := observability.RequestIDMiddleware(requestLoggingMiddleware(logger, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setRequestLogModel(r, "gpt-5")
		setRequestLogReasoningEffort(r, "high")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ok"))
	})))
	req := httptest.NewRequest(http.MethodPost, "/v1/models?secret=not-logged", nil)
	req.Header.Set("X-Request-ID", "req-test")
	req.Header.Set("User-Agent", "unit-test")
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)
	logLine := buf.String()
	for _, want := range []string{`"msg":"request completed"`, `"request_id":"req-test"`, `"method":"POST"`, `"path":"/v1/models"`, `"model":"gpt-5"`, `"reasoning_effort":"high"`, `"status":201`, `"bytes":2`, `"user_agent":"unit-test"`} {
		if !strings.Contains(logLine, want) {
			t.Fatalf("log line missing %s: %s", want, logLine)
		}
	}
	if strings.Contains(logLine, "not-logged") {
		t.Fatalf("request logging should not include query strings: %s", logLine)
	}
}

func TestRequestLoggingMiddlewareContentLoggingDisabled(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := observability.RequestIDMiddleware(requestLoggingMiddleware(logger, false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte("response-secret"))
	})))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("request-secret"))
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)
	logLines := buf.String()
	for _, secret := range []string{"request-secret", "response-secret", "request_body", "response_body"} {
		if strings.Contains(logLines, secret) {
			t.Fatalf("content logging disabled but log contains %q: %s", secret, logLines)
		}
	}
}

func TestRequestLoggingMiddlewareContentLoggingEnabled(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := observability.RequestIDMiddleware(requestLoggingMiddleware(logger, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		if string(body) != "request-secret" {
			t.Fatalf("request body = %q, want request-secret", body)
		}
		_, _ = w.Write([]byte("response-secret"))
	})))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("request-secret"))
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)
	logLines := buf.String()
	for _, want := range []string{`"request_body":"request-secret"`, `"response_body":"response-secret"`} {
		if !strings.Contains(logLines, want) {
			t.Fatalf("content log missing %s: %s", want, logLines)
		}
	}
}

func TestRequestLoggingMiddlewareContentLoggingTruncates(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	requestBody := strings.Repeat("a", maxLoggedBodyBytes+1)
	responseBody := strings.Repeat("b", maxLoggedBodyBytes+1)
	h := observability.RequestIDMiddleware(requestLoggingMiddleware(logger, true, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(responseBody))
	})))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody))
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 {
		t.Fatal("expected log lines")
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
		t.Fatalf("decode completed log: %v", err)
	}
	if got := len(entry["request_body"].(string)); got != maxLoggedBodyBytes {
		t.Fatalf("request_body length = %d, want %d", got, maxLoggedBodyBytes)
	}
	if got := len(entry["response_body"].(string)); got != maxLoggedBodyBytes {
		t.Fatalf("response_body length = %d, want %d", got, maxLoggedBodyBytes)
	}
	if entry["request_body_truncated"] != true || entry["response_body_truncated"] != true {
		t.Fatalf("truncation flags = request:%#v response:%#v", entry["request_body_truncated"], entry["response_body_truncated"])
	}
}

func TestGenerationLoggingUsesGatewayResolvedReasoningEffort(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	gw := &resolvingChatGateway{resolvedEffort: "medium"}
	s := New(config.Config{DefaultReasoningEffort: "minimal"}, gw, logger)
	body := strings.NewReader(`{"model":"claude-opus-4.8","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if gw.resolveCalls != 1 || gw.resolveModel != "claude-opus-4.8" || gw.resolveRequested != "" || gw.resolveDefault != "minimal" {
		t.Fatalf("ResolveReasoningEffort called %d times with model=%q requested=%q default=%q", gw.resolveCalls, gw.resolveModel, gw.resolveRequested, gw.resolveDefault)
	}
	if !gw.got.ReasoningEffortResolved || gw.got.ResolvedReasoningEffort != "medium" {
		t.Fatalf("gateway request resolved effort = (%t, %q), want (true, medium)", gw.got.ReasoningEffortResolved, gw.got.ResolvedReasoningEffort)
	}
	logLines := buf.String()
	for _, want := range []string{`"msg":"generation started"`, `"endpoint":"chat.completions"`, `"model":"claude-opus-4.8"`, `"reasoning_effort":""`, `"reasoning_effort_resolved":"medium"`, `"msg":"request completed"`} {
		if !strings.Contains(logLines, want) {
			t.Fatalf("log lines missing %s: %s", want, logLines)
		}
	}
	if got := strings.Count(logLines, `"reasoning_effort_resolved":"medium"`); got != 2 {
		t.Fatalf("reasoning_effort_resolved log count = %d, want 2: %s", got, logLines)
	}
	if got := strings.Count(logLines, `"endpoint":"chat.completions"`); got != 2 {
		t.Fatalf("endpoint log count = %d, want 2: %s", got, logLines)
	}
}

func TestChatContinuationLoggingDoesNotResolveReasoningEffort(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	gw := &resolvingChatGateway{resolvedEffort: "medium"}
	s := New(config.Config{DefaultReasoningEffort: "minimal"}, gw, logger)
	body := strings.NewReader(`{"model":"gpt-5","reasoning_effort":"high","messages":[{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if gw.resolveCalls != 0 {
		t.Fatalf("ResolveReasoningEffort calls = %d, want 0 for tool continuations", gw.resolveCalls)
	}
	if !gw.continueCalled {
		t.Fatal("expected ContinueChatToolCalls to be called")
	}
	logLines := buf.String()
	for _, want := range []string{`"msg":"generation started"`, `"endpoint":"chat.completions"`, `"continuation":true`, `"reasoning_effort":"high"`} {
		if !strings.Contains(logLines, want) {
			t.Fatalf("log lines missing %s: %s", want, logLines)
		}
	}
	if strings.Contains(logLines, "reasoning_effort_resolved") {
		t.Fatalf("continuation log should not include resolved reasoning effort: %s", logLines)
	}
	if got := strings.Count(logLines, `"continuation":true`); got != 2 {
		t.Fatalf("continuation log count = %d, want 2: %s", got, logLines)
	}
}

func TestChatRequestPassesReasoningHistoryAndImageInput(t *testing.T) {
	gw := &captureChatGateway{}
	s := New(config.Config{}, gw, slog.Default())
	body := strings.NewReader(`{
		"model":"gpt-5",
		"reasoning_effort":"high",
		"messages":[
			{"role":"system","content":"Be concise."},
			{"role":"user","content":"Remember alpha."},
			{"role":"assistant","content":"I remember alpha."},
			{"role":"user","content":[
				{"type":"text","text":"What did I ask you to remember, and what is attached?"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA","detail":"low"}}
			]}
		]
	}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if gw.got.ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort = %q, want high", gw.got.ReasoningEffort)
	}
	if gw.got.Instructions != "System:\nBe concise." {
		t.Fatalf("Instructions = %q, want folded system instructions", gw.got.Instructions)
	}
	if len(gw.got.History) != 2 || gw.got.History[0].Role != "user" || gw.got.History[1].Role != "assistant" {
		t.Fatalf("History = %#v, want prior user+assistant messages", gw.got.History)
	}
	prompt, err := gw.got.FinalUser.Prompt()
	if err != nil {
		t.Fatal(err)
	}
	if prompt.Text != "What did I ask you to remember, and what is attached?" {
		t.Fatalf("FinalUser text = %q", prompt.Text)
	}
	if len(prompt.Images) != 1 || prompt.Images[0].URL != "data:image/png;base64,AAAA" || prompt.Images[0].Detail != "low" {
		t.Fatalf("FinalUser images = %#v, want one low-detail data image", prompt.Images)
	}
}

func TestChatRequestPassesConfiguredDefaultReasoningEffort(t *testing.T) {
	gw := &captureChatGateway{}
	s := New(config.Config{DefaultReasoningEffort: "medium"}, gw, slog.Default())
	body := strings.NewReader(`{
		"model":"gpt-5",
		"messages":[{"role":"user","content":"hi"}]
	}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if gw.got.ReasoningEffort != "" {
		t.Fatalf("ReasoningEffort = %q, want explicit request effort empty", gw.got.ReasoningEffort)
	}
	if gw.got.DefaultReasoningEffort != "medium" {
		t.Fatalf("DefaultReasoningEffort = %q, want medium", gw.got.DefaultReasoningEffort)
	}
}

func TestChatStreamWithToolCallAndIncludeUsageUsesOpenAIChunkShape(t *testing.T) {
	gw := &streamChatGateway{}
	s := New(config.Config{}, gw, slog.Default())
	body := strings.NewReader(`{
		"model":"gpt-5",
		"stream":true,
		"stream_options":{"include_usage":true},
		"messages":[{"role":"user","content":"look up alpha"}],
		"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]
	}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if !gw.got.IncludeUsageChunk {
		t.Fatal("IncludeUsageChunk = false, want true")
	}
	out := w.Body.String()
	for _, want := range []string{
		`"role":"assistant"`,
		`"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"alpha\"}"}}]`,
		`"finish_reason":"tool_calls"`,
		`"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}`,
		`data: [DONE]`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream missing %s:\n%s", want, out)
		}
	}
}

func TestResponsesRequestPassesNestedReasoningImageInputAndIgnoresMCPTool(t *testing.T) {
	gw := &captureResponseGateway{}
	s := New(config.Config{}, gw, slog.Default())
	body := strings.NewReader(`{
		"model":"gpt-5",
		"reasoning":{"effort":"low"},
		"tools":[{"type":"mcp","server_label":"test-mcp","server_url":"https://example.invalid/mcp"}],
		"input":[{"type":"message","role":"user","content":[
			{"type":"input_text","text":"Describe the attachment."},
			{"type":"input_image","image_url":"data:image/png;base64,AAAA","detail":"high"}
		]}]
	}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if gw.got.ReasoningEffort != "low" {
		t.Fatalf("ReasoningEffort = %q, want low", gw.got.ReasoningEffort)
	}
	if len(gw.got.Tools) != 0 {
		t.Fatalf("gateway tools = %#v, want MCP provider tool ignored in permissive mode", gw.got.Tools)
	}
	if gw.got.Input.Text != "Describe the attachment." {
		t.Fatalf("Input.Text = %q", gw.got.Input.Text)
	}
	if len(gw.got.Input.Images) != 1 || gw.got.Input.Images[0].URL != "data:image/png;base64,AAAA" || gw.got.Input.Images[0].Detail != "high" {
		t.Fatalf("Input.Images = %#v, want one high-detail data image", gw.got.Input.Images)
	}
}

func TestResponsesRequestPassesConfiguredDefaultReasoningEffort(t *testing.T) {
	gw := &captureResponseGateway{}
	s := New(config.Config{DefaultReasoningEffort: "high"}, gw, slog.Default())
	body := strings.NewReader(`{
		"model":"gpt-5",
		"input":"hi"
	}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if gw.got.ReasoningEffort != "" {
		t.Fatalf("ReasoningEffort = %q, want explicit request effort empty", gw.got.ReasoningEffort)
	}
	if gw.got.DefaultReasoningEffort != "high" {
		t.Fatalf("DefaultReasoningEffort = %q, want high", gw.got.DefaultReasoningEffort)
	}
}

func TestStrictResponsesRejectsMCPProviderTool(t *testing.T) {
	s := New(config.Config{StrictCompat: true}, &captureResponseGateway{}, slog.Default())
	body := strings.NewReader(`{"model":"gpt-5","tools":[{"type":"mcp","server_label":"test-mcp","server_url":"https://example.invalid/mcp"}],"input":"hi"}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "only function tools are supported") {
		t.Fatalf("unexpected error body: %s", w.Body.String())
	}
}

func TestResponsesRequestPassesPreviousResponseIDStoreAndFunctionOutputs(t *testing.T) {
	gw := &captureResponseGateway{}
	s := New(config.Config{}, gw, slog.Default())
	body := strings.NewReader(`{
		"model":"gpt-5",
		"previous_response_id":"resp_previous",
		"store":false,
		"input":[
			{"type":"function_call_output","call_id":"call_1","output":{"answer":42}}
		]
	}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if gw.got.PreviousResponseID != "resp_previous" {
		t.Fatalf("PreviousResponseID = %q, want resp_previous", gw.got.PreviousResponseID)
	}
	if gw.got.Store || !gw.got.StoreSet {
		t.Fatalf("Store/StoreSet = %v/%v, want false/true", gw.got.Store, gw.got.StoreSet)
	}
	if got := gw.got.FunctionOutputs["call_1"]; got != `{"answer":42}` {
		t.Fatalf("FunctionOutputs[call_1] = %q, want compact JSON object", got)
	}
	if gw.got.Input.Text != "" || len(gw.got.Input.Images) != 0 {
		t.Fatalf("Input = %#v, want function-output continuation without prompt", gw.got.Input)
	}
}

func TestResponsesStreamEmitsFunctionCallEventsAndCompletedResponse(t *testing.T) {
	gw := &functionCallStreamGateway{}
	s := New(config.Config{}, gw, slog.Default())
	body := strings.NewReader(`{
		"model":"gpt-5",
		"stream":true,
		"input":"call lookup",
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}]
	}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	out := w.Body.String()
	for _, want := range []string{
		`event: response.created`,
		`event: response.output_item.added`,
		`"type":"function_call"`,
		`"call_id":"call_1"`,
		`event: response.function_call_arguments.delta`,
		`"delta":"{\"q\":\"alpha\"}"`,
		`event: response.function_call_arguments.done`,
		`"arguments":"{\"q\":\"alpha\"}"`,
		`event: response.output_item.done`,
		`event: response.completed`,
		`data: [DONE]`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("response stream missing %s:\n%s", want, out)
		}
	}
}

func TestResponsesGetDeleteHTTPContract(t *testing.T) {
	gw := &statefulResponseGateway{resp: &openai.Response{ID: "resp_1", Object: openai.ObjectResponse, CreatedAt: openai.UnixNow(), Status: "completed", Model: "gpt-5", OutputText: "ok", Output: []openai.ResponseOutputItem{}, ParallelToolCalls: true, Store: true}}
	s := New(config.Config{}, gw, slog.Default())

	get := httptest.NewRecorder()
	s.Handler().ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/responses/resp_1", nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"id":"resp_1"`) {
		t.Fatalf("GET status/body = %d %s, want response", get.Code, get.Body.String())
	}

	invalid := httptest.NewRecorder()
	s.Handler().ServeHTTP(invalid, httptest.NewRequest(http.MethodGet, "/v1/responses/resp_1/extra", nil))
	if invalid.Code != http.StatusNotFound {
		t.Fatalf("nested GET status = %d, want 404: %s", invalid.Code, invalid.Body.String())
	}

	del := httptest.NewRecorder()
	s.Handler().ServeHTTP(del, httptest.NewRequest(http.MethodDelete, "/v1/responses/resp_1", nil))
	if del.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d, want 200: %s", del.Code, del.Body.String())
	}
	for _, want := range []string{`"id":"resp_1"`, `"object":"response.deleted"`, `"deleted":true`} {
		if !strings.Contains(del.Body.String(), want) {
			t.Fatalf("DELETE body missing %s: %s", want, del.Body.String())
		}
	}

	missing := httptest.NewRecorder()
	s.Handler().ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/v1/responses/resp_1", nil))
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"code":"not_found"`) {
		t.Fatalf("GET after DELETE status/body = %d %s, want not_found", missing.Code, missing.Body.String())
	}
}

func TestResponsesStreamAcceptsCodexRequestShape(t *testing.T) {
	gw := &codexStreamGateway{}
	s := New(config.Config{}, gw, slog.Default())
	body := strings.NewReader(`{"model":"gpt-5.5","stream":true,"include":["reasoning.encrypted_content"],"reasoning":{"effort":"medium","summary":"auto"},"text":{"verbosity":"low"},"tools":[{"type":"function","name":"exec_command","description":"run","parameters":{"type":"object","properties":{}}},{"type":"custom","name":"apply_patch"}],"instructions":"base","input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"desktop context"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	rr := httptest.NewRecorder()

	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if gw.got.ReasoningEffort != "medium" {
		t.Fatalf("ReasoningEffort = %q, want medium", gw.got.ReasoningEffort)
	}
	if len(gw.got.Tools) != 1 || gw.got.Tools[0].Function.Name != "exec_command" {
		t.Fatalf("gateway tools = %#v, want exec_command only", gw.got.Tools)
	}
	if gw.got.Instructions != "base\n\nDeveloper:\ndesktop context" {
		t.Fatalf("Instructions = %q, want developer context folded in", gw.got.Instructions)
	}
	if gw.got.Input.Text != "hi" {
		t.Fatalf("Input.Text = %q, want hi", gw.got.Input.Text)
	}
	out := rr.Body.String()
	created := strings.Index(out, "event: response.created")
	added := strings.Index(out, "event: response.output_item.added")
	delta := strings.Index(out, "event: response.output_text.delta")
	completed := strings.Index(out, "event: response.completed")
	if created < 0 || added < 0 || delta < 0 || completed < 0 || !(created < added && added < delta && delta < completed) {
		t.Fatalf("unexpected SSE order:\n%s", out)
	}
}

func TestStreamedMessageItemKeepsTextAtStableIndex(t *testing.T) {
	resp := &openai.Response{Output: []openai.ResponseOutputItem{{ID: "fc_call_1", Type: "function_call", Status: "completed", CallID: "call_1", Name: "lookup", Arguments: `{}`}}}
	item, idx := streamedMessageItem(resp, "msg_stream", "hello")
	if idx != 0 || item == nil || item.ID != "msg_stream" || item.Type != "message" {
		t.Fatalf("streamedMessageItem = (%#v, %d), want message at index 0", item, idx)
	}
	if len(resp.Output) != 2 || resp.Output[0].Type != "message" || resp.Output[1].Type != "function_call" {
		t.Fatalf("unexpected output order: %#v", resp.Output)
	}
	if resp.Output[1].CallID != "call_1" {
		t.Fatalf("function call was not preserved: %#v", resp.Output[1])
	}
}

func TestHTTPValidationErrorsAreOpenAIShaped(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		body  string
		param string
	}{
		{
			name:  "chat parallel tool calls",
			path:  "/v1/chat/completions",
			body:  `{"model":"gpt-5","parallel_tool_calls":true,"messages":[{"role":"user","content":"hi"}]}`,
			param: "parallel_tool_calls",
		},
		{
			name:  "responses parallel tool calls false",
			path:  "/v1/responses",
			body:  `{"model":"gpt-5","parallel_tool_calls":false,"input":"hi"}`,
			param: "parallel_tool_calls",
		},
		{
			name:  "forced function tool choice",
			path:  "/v1/chat/completions",
			body:  `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup"}}],"tool_choice":{"type":"function","function":{"name":"lookup"}}}`,
			param: "tool_choice",
		},
		{
			name:  "responses text format",
			path:  "/v1/responses",
			body:  `{"model":"gpt-5","text":{"format":{"type":"json_object"}},"input":"hi"}`,
			param: "text.format",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := New(config.Config{}, &captureResponseGateway{}, slog.Default())
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			var env openai.ErrorEnvelope
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatal(err)
			}
			if env.Error.Type != "invalid_request_error" || env.Error.Param != tc.param {
				t.Fatalf("error = %#v, want invalid_request_error param %q", env.Error, tc.param)
			}
		})
	}
}

func TestAuthMiddlewareProtectsV1ButNotHealth(t *testing.T) {
	s := New(config.Config{APIKey: "secret"}, modelsGateway{models: []copilotgw.Model{{ID: "gpt-5"}}}, slog.Default())

	health := httptest.NewRecorder()
	s.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200: %s", health.Code, health.Body.String())
	}

	unauth := httptest.NewRecorder()
	s.Handler().ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if unauth.Code != http.StatusUnauthorized || !strings.Contains(unauth.Body.String(), `"code":"invalid_api_key"`) {
		t.Fatalf("unauth status/body = %d %s, want invalid_api_key", unauth.Code, unauth.Body.String())
	}

	wrongReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	wrongReq.Header.Set("Authorization", "Bearer wrong")
	wrong := httptest.NewRecorder()
	s.Handler().ServeHTTP(wrong, wrongReq)
	if wrong.Code != http.StatusUnauthorized {
		t.Fatalf("wrong auth status = %d, want 401", wrong.Code)
	}

	okReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	okReq.Header.Set("Authorization", "Bearer secret")
	ok := httptest.NewRecorder()
	s.Handler().ServeHTTP(ok, okReq)
	if ok.Code != http.StatusOK {
		t.Fatalf("auth status = %d, want 200: %s", ok.Code, ok.Body.String())
	}
}

func TestToolOutputsSerializeObjectsAndArrays(t *testing.T) {
	objectContent := openai.Content{Present: true, Raw: json.RawMessage(`{"answer":42}`)}
	if got, err := toolOutputFromContent(objectContent); err != nil || got != `{"answer":42}` {
		t.Fatalf("toolOutputFromContent object = %q, %v; want compact object", got, err)
	}
	arrayContent := openai.Content{Present: true, Raw: json.RawMessage(`["a","b"]`)}
	if got, err := toolOutputFromContent(arrayContent); err != nil || got != `["a","b"]` {
		t.Fatalf("toolOutputFromContent array = %q, %v; want compact array", got, err)
	}
	scalarContent := openai.Content{Present: true, Raw: json.RawMessage(`42`)}
	if _, err := toolOutputFromContent(scalarContent); err == nil {
		t.Fatal("expected scalar tool output rejection")
	}

	_, outputs, _, err := parseResponsesInput(json.RawMessage(`[{"type":"function_call_output","call_id":"call_obj","output":{"ok":true}},{"type":"function_call_output","call_id":"call_arr","output":[1,2]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if outputs["call_obj"] != `{"ok":true}` || outputs["call_arr"] != `[1,2]` {
		t.Fatalf("responses function outputs = %#v, want compact JSON object and array", outputs)
	}
	_, _, _, err = parseResponsesInput(json.RawMessage(`[{"type":"function_call_output","call_id":"call_scalar","output":42}]`))
	if err == nil {
		t.Fatal("expected scalar function_call_output rejection")
	}
}

func TestParseResponsesInputRejectsDuplicateFunctionOutputs(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"function_call_output","call_id":"call_1","output":"a"},
		{"type":"function_call_output","call_id":"call_1","output":"b"}
	]`)
	_, _, _, err := parseResponsesInput(raw)
	if err == nil {
		t.Fatal("expected duplicate call_id rejection")
	}
}

func TestParseResponsesInputFoldsDeveloperMessagesIntoInstructions(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"message","role":"developer","content":[{"type":"input_text","text":"desktop context"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}
	]`)
	prompt, outputs, instructions, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if outputs != nil {
		t.Fatalf("unexpected outputs: %#v", outputs)
	}
	if instructions != "Developer:\ndesktop context" {
		t.Fatalf("instructions = %q, want developer context", instructions)
	}
	if prompt.Text != "hello" {
		t.Fatalf("prompt text = %q, want hello", prompt.Text)
	}
}

func TestParseResponsesInputSerializesAssistantHistoryAsTranscript(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"message","role":"user","content":"hi"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},
		{"type":"message","role":"user","content":"again"}
	]`)
	prompt, outputs, instructions, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if outputs != nil || instructions != "" {
		t.Fatalf("unexpected outputs/instructions: %#v %q", outputs, instructions)
	}
	want := "User:\nhi\n\nAssistant:\nhello\n\nUser:\nagain"
	if prompt.Text != want {
		t.Fatalf("prompt text = %q, want %q", prompt.Text, want)
	}
}

func TestParseResponsesInputAcceptsImageParts(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"message","role":"user","content":[
			{"type":"input_text","text":"describe"},
			{"type":"input_image","image_url":"data:image/png;base64,AAAA","detail":"high"}
		]}
	]`)
	prompt, outputs, instructions, err := parseResponsesInput(raw)
	if err != nil {
		t.Fatal(err)
	}
	if outputs != nil {
		t.Fatalf("unexpected outputs: %#v", outputs)
	}
	if instructions != "" {
		t.Fatalf("unexpected instructions: %q", instructions)
	}
	if prompt.Text != "describe" {
		t.Fatalf("unexpected prompt text %q", prompt.Text)
	}
	if len(prompt.Images) != 1 || prompt.Images[0].URL != "data:image/png;base64,AAAA" || prompt.Images[0].Detail != "high" {
		t.Fatalf("unexpected images: %#v", prompt.Images)
	}
}
