package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/config"
)

// Clients that serialize an explicit None (openai-python) send `"store": null`
// rather than omitting the key. A null must read as "not set" everywhere the
// proxy derives a boolean from field presence.

func TestResponsesExplicitNullOptionalFieldsAreNotSet(t *testing.T) {
	gw := &captureResponseGateway{}
	s := New(config.Config{}, gw, slog.Default())
	body := strings.NewReader(`{
		"model":"gpt-5",
		"input":"hi",
		"stream":null,
		"store":null,
		"tools":null,
		"tool_choice":null,
		"parallel_tool_calls":null,
		"temperature":null,
		"max_output_tokens":null,
		"truncation":null,
		"reasoning":null,
		"text":null,
		"include":null,
		"metadata":null,
		"user":null
	}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	// stream:null must not select the SSE path.
	if !strings.Contains(w.Body.String(), `"object":"response"`) {
		t.Fatalf("stream:null should produce a single JSON response: %s", w.Body.String())
	}
	if gw.got.StoreSet {
		t.Fatalf(`StoreSet = true for {"store":null}, want false`)
	}
	if !gw.got.Store {
		t.Fatalf("Store = false, want the unset default of true")
	}
	if gw.got.ToolsSet {
		t.Fatalf(`ToolsSet = true for {"tools":null}, want false`)
	}
	if len(gw.got.Tools) != 0 {
		t.Fatalf("Tools = %#v, want none", gw.got.Tools)
	}
	if gw.got.ToolChoiceNone {
		t.Fatalf(`ToolChoiceNone = true for {"tool_choice":null}, want false`)
	}
}

func TestResponsesExplicitNullOptionalFieldsAcceptedInStrictMode(t *testing.T) {
	gw := &captureResponseGateway{}
	s := New(config.Config{StrictCompat: true}, gw, slog.Default())
	body := strings.NewReader(`{"model":"gpt-5","input":"hi","temperature":null,"top_p":null,"metadata":null,"user":null,"include":null,"reasoning":null,"text":null,"service_tier":null}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestResponsesExplicitStoreAndToolsStaySet(t *testing.T) {
	gw := &captureResponseGateway{}
	s := New(config.Config{}, gw, slog.Default())
	body := strings.NewReader(`{"model":"gpt-5","input":"hi","store":false,"tools":[],"tool_choice":"none"}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if !gw.got.StoreSet || gw.got.Store {
		t.Fatalf("Store/StoreSet = %v/%v, want false/true", gw.got.Store, gw.got.StoreSet)
	}
	if !gw.got.ToolsSet {
		t.Fatalf(`ToolsSet = false for {"tools":[]}, want true`)
	}
	if !gw.got.ToolChoiceNone {
		t.Fatalf(`ToolChoiceNone = false for {"tool_choice":"none"}, want true`)
	}
}

func TestChatCompletionsAcceptsExplicitNullOptionalFields(t *testing.T) {
	gw := &captureChatGateway{}
	s := New(config.Config{}, gw, slog.Default())
	body := strings.NewReader(`{
		"model":"gpt-5",
		"messages":[{"role":"user","content":"hi"}],
		"stream":null,
		"stop":null,
		"n":null,
		"max_tokens":null,
		"max_completion_tokens":null,
		"response_format":null,
		"logit_bias":null,
		"logprobs":null,
		"top_logprobs":null,
		"audio":null,
		"modalities":null,
		"prediction":null,
		"functions":null,
		"function_call":null,
		"tools":null,
		"tool_choice":null,
		"parallel_tool_calls":null,
		"temperature":null,
		"user":null
	}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"object":"chat.completion"`) {
		t.Fatalf("stream:null should produce a single JSON completion: %s", w.Body.String())
	}
	if len(gw.got.Tools) != 0 {
		t.Fatalf("Tools = %#v, want none", gw.got.Tools)
	}
}

func TestChatCompletionsStillRejectsNonNullUnsupportedFields(t *testing.T) {
	s := New(config.Config{}, &captureChatGateway{}, slog.Default())
	body := strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"stop":["done"]}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stop sequences are not supported") {
		t.Fatalf("unexpected error body: %s", w.Body.String())
	}
}
