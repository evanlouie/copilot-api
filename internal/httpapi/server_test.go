package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestRequestLoggingMiddlewareLogsMetadata(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	h := observability.RequestIDMiddleware(requestLoggingMiddleware(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
