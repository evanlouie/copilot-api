package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

type reasoningStreamChatGateway struct {
	copilotgw.Gateway
}

func (g *reasoningStreamChatGateway) StreamChat(_ context.Context, req copilotgw.ChatRequest) (<-chan copilotgw.StreamEvent, error) {
	ch := make(chan copilotgw.StreamEvent, 8)
	go func() {
		defer close(ch)
		ch <- copilotgw.StreamEvent{Kind: "reasoning_delta", Delta: "think-", ReasoningID: "rid-1"}
		ch <- copilotgw.StreamEvent{Kind: "reasoning_delta", Delta: "more", ReasoningID: "rid-1"}
		ch <- copilotgw.StreamEvent{Kind: "delta", Delta: "answer"}
		ch <- copilotgw.StreamEvent{Kind: "result", Result: &copilotgw.TurnResult{
			ID:              req.OpenAIID,
			Created:         openai.UnixNow(),
			Model:           req.Model,
			Text:            "answer",
			Reasoning:       "think-more",
			ReasoningOpaque: "sig-blob",
			ReasoningID:     "rid-1",
			FinishReason:    "stop",
		}}
	}()
	return ch, nil
}

type reasoningChatGateway struct {
	copilotgw.Gateway
}

func (g *reasoningChatGateway) Chat(_ context.Context, req copilotgw.ChatRequest) (*copilotgw.TurnResult, error) {
	return &copilotgw.TurnResult{
		ID:              req.OpenAIID,
		Created:         openai.UnixNow(),
		Model:           req.Model,
		Text:            "answer",
		Reasoning:       "because",
		ReasoningOpaque: "sig-blob",
		ReasoningID:     "rid-1",
		FinishReason:    "stop",
	}, nil
}

type reasoningResponseStreamGateway struct {
	copilotgw.Gateway
}

func (g *reasoningResponseStreamGateway) StreamResponse(_ context.Context, req copilotgw.ResponseRequest) (<-chan copilotgw.ResponseStreamEvent, error) {
	ch := make(chan copilotgw.ResponseStreamEvent, 8)
	go func() {
		defer close(ch)
		ch <- copilotgw.ResponseStreamEvent{Kind: "reasoning_delta", Delta: "thinking", ReasoningID: "rid-1"}
		ch <- copilotgw.ResponseStreamEvent{Kind: "delta", Delta: "answer"}
		turn := &copilotgw.TurnResult{Text: "answer", Reasoning: "thinking", ReasoningID: "rid-1"}
		resp := responseForReasoningTest(req, turn)
		ch <- copilotgw.ResponseStreamEvent{Kind: "response", Response: resp}
	}()
	return ch, nil
}

func responseForReasoningTest(req copilotgw.ResponseRequest, turn *copilotgw.TurnResult) *openai.Response {
	return &openai.Response{
		ID:         req.ResponseID,
		Object:     openai.ObjectResponse,
		CreatedAt:  openai.UnixNow(),
		Status:     "completed",
		Model:      req.Model,
		OutputText: turn.Text,
		Output: []openai.ResponseOutputItem{
			{ID: "rs_rid-1", Type: "reasoning", Status: "completed", Summary: []openai.ResponseReasoningSummary{{Type: "summary_text", Text: turn.Reasoning}}},
			{ID: "msg_1", Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: turn.Text}}},
		},
		ParallelToolCalls: true,
		Store:             req.Store,
	}
}

func TestChatStreamEmitsReasoningDeltasBeforeContent(t *testing.T) {
	s := New(config.Config{}, &reasoningStreamChatGateway{}, slog.Default())
	body := strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	reasoningIdx := strings.Index(out, `"reasoning":"think-"`)
	reasoningContentIdx := strings.Index(out, `"reasoning_content":"think-"`)
	contentIdx := strings.Index(out, `"content":"answer"`)
	detailsIdx := strings.Index(out, `"reasoning_details"`)
	finishIdx := strings.Index(out, `"finish_reason":"stop"`)
	if reasoningIdx < 0 || reasoningContentIdx < 0 || contentIdx < 0 || detailsIdx < 0 || finishIdx < 0 {
		t.Fatalf("missing expected reasoning/content/details chunks:\n%s", out)
	}
	if !(reasoningIdx < contentIdx && contentIdx < detailsIdx && detailsIdx < finishIdx) {
		t.Fatalf("expected reasoning < content < reasoning_details < finish ordering:\n%s", out)
	}
	if !strings.Contains(out, `"signature":"sig-blob"`) || !strings.Contains(out, `"format":"anthropic-claude-v1"`) {
		t.Fatalf("terminal reasoning_details missing signature/format:\n%s", out)
	}
}

func TestChatNonStreamingAttachesReasoning(t *testing.T) {
	s := New(config.Config{}, &reasoningChatGateway{}, slog.Default())
	body := strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var completion openai.ChatCompletion
	if err := json.Unmarshal(w.Body.Bytes(), &completion); err != nil {
		t.Fatal(err)
	}
	msg := completion.Choices[0].Message
	if msg.Reasoning != "because" || msg.ReasoningContent != "because" {
		t.Fatalf("reasoning fields = %q / %q, want because", msg.Reasoning, msg.ReasoningContent)
	}
	if len(msg.ReasoningDetails) != 1 || msg.ReasoningDetails[0].Signature != "sig-blob" || msg.ReasoningDetails[0].Format != "anthropic-claude-v1" {
		t.Fatalf("reasoning_details = %#v", msg.ReasoningDetails)
	}
}

func TestChatReasoningEmissionPolicyNarrowing(t *testing.T) {
	t.Run("reasoning only", func(t *testing.T) {
		s := New(config.Config{ReasoningEmission: "reasoning"}, &reasoningChatGateway{}, slog.Default())
		out := postChat(t, s)
		if !strings.Contains(out, `"reasoning":"because"`) {
			t.Fatalf("expected reasoning field: %s", out)
		}
		if strings.Contains(out, `"reasoning_content"`) {
			t.Fatalf("reasoning_content should be suppressed: %s", out)
		}
		if !strings.Contains(out, `"reasoning_details"`) {
			t.Fatalf("reasoning_details should still be present: %s", out)
		}
	})
	t.Run("off", func(t *testing.T) {
		s := New(config.Config{ReasoningEmission: "off"}, &reasoningChatGateway{}, slog.Default())
		out := postChat(t, s)
		for _, banned := range []string{`"reasoning"`, `"reasoning_content"`, `"reasoning_details"`} {
			if strings.Contains(out, banned) {
				t.Fatalf("policy off must suppress %s: %s", banned, out)
			}
		}
	})
}

func postChat(t *testing.T, s *Server) string {
	t.Helper()
	body := strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

func TestChatAcceptsParallelToolCallsTrue(t *testing.T) {
	gw := &captureChatGateway{}
	s := New(config.Config{}, gw, slog.Default())
	body := strings.NewReader(`{"model":"gpt-5","parallel_tool_calls":true,"messages":[{"role":"user","content":"hi"}]}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (parallel_tool_calls=true accepted): %s", w.Code, w.Body.String())
	}
}

func TestResponsesStreamEmitsReasoningSummaryBeforeMessage(t *testing.T) {
	s := New(config.Config{}, &reasoningResponseStreamGateway{}, slog.Default())
	body := strings.NewReader(`{"model":"claude-sonnet-4.6","stream":true,"input":"hi"}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	out := w.Body.String()
	reasoningDelta := strings.Index(out, "event: response.reasoning_summary_text.delta")
	reasoningDone := strings.Index(out, "event: response.reasoning_summary_text.done")
	textDelta := strings.Index(out, "event: response.output_text.delta")
	completed := strings.Index(out, "event: response.completed")
	if reasoningDelta < 0 || reasoningDone < 0 || textDelta < 0 || completed < 0 {
		t.Fatalf("missing reasoning/text events:\n%s", out)
	}
	if !(reasoningDelta < reasoningDone && reasoningDone < textDelta && textDelta < completed) {
		t.Fatalf("expected reasoning summary events before text events:\n%s", out)
	}
	// The message item shifts to output index 1 because reasoning occupies 0.
	if !strings.Contains(out, `"output_index":1`) {
		t.Fatalf("expected message item at output_index 1 when reasoning present:\n%s", out)
	}
	if !strings.Contains(out, `"summary_index":0`) {
		t.Fatalf("expected reasoning summary_index:\n%s", out)
	}
}
