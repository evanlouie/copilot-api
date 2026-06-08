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

type reasoningResponseGateway struct {
	copilotgw.Gateway
	got copilotgw.ResponseRequest
}

func (g *reasoningResponseGateway) CreateResponse(_ context.Context, req copilotgw.ResponseRequest) (*copilotgw.ResponseResult, error) {
	g.got = req
	turn := &copilotgw.TurnResult{Text: "answer", Reasoning: "thinking", ReasoningEncrypted: "enc-blob", ReasoningID: "rid-1"}
	resp := &openai.Response{
		ID:                req.ResponseID,
		Object:            openai.ObjectResponse,
		CreatedAt:         openai.UnixNow(),
		Status:            "completed",
		Model:             req.Model,
		OutputText:        turn.Text,
		Output:            []openai.ResponseOutputItem{{ID: "msg_1", Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: turn.Text}}}},
		ParallelToolCalls: true,
		Store:             req.Store,
	}
	resp.Output = append([]openai.ResponseOutputItem{{ID: "rs_rid-1", Type: "reasoning", Status: "completed", Summary: []openai.ResponseReasoningSummary{{Type: "summary_text", Text: turn.Reasoning}}, EncryptedContent: turn.ReasoningEncrypted}}, resp.Output...)
	return &copilotgw.ResponseResult{Response: resp}, nil
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
	chunks := parseChatStreamChunks(t, w.Body.String())
	if len(chunks) < 6 {
		t.Fatalf("stream produced %d chunks, want role + reasoning + content + details + finish: %#v", len(chunks), chunks)
	}
	if chunks[0].Choices[0].Delta.Role != "assistant" {
		t.Fatalf("first chunk = %#v, want assistant role", chunks[0])
	}
	if chunks[1].Choices[0].Delta.Reasoning != "think-" || chunks[1].Choices[0].Delta.ReasoningContent != "think-" {
		t.Fatalf("first reasoning chunk = %#v", chunks[1])
	}
	if chunks[2].Choices[0].Delta.Reasoning != "more" || chunks[2].Choices[0].Delta.ReasoningContent != "more" {
		t.Fatalf("second reasoning chunk = %#v", chunks[2])
	}
	if chunks[3].Choices[0].Delta.Content != "answer" {
		t.Fatalf("content chunk = %#v", chunks[3])
	}
	details := chunks[4].Choices[0].Delta.ReasoningDetails
	if len(details) != 1 || details[0].Signature != "sig-blob" || details[0].Format != "anthropic-claude-v1" {
		t.Fatalf("terminal reasoning_details missing signature/format: %#v", chunks[4])
	}
	finish := chunks[5].Choices[0].FinishReason
	if finish == nil || *finish != "stop" {
		t.Fatalf("finish chunk = %#v, want stop", chunks[5])
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

func TestChatStreamReasoningEmissionPolicy(t *testing.T) {
	collect := func(emission string) (reasoning, reasoningContent bool, details bool) {
		s := New(config.Config{ReasoningEmission: emission}, &reasoningStreamChatGateway{}, slog.Default())
		body := strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		for _, chunk := range parseChatStreamChunks(t, w.Body.String()) {
			for _, ch := range chunk.Choices {
				if ch.Delta.Reasoning != "" {
					reasoning = true
				}
				if ch.Delta.ReasoningContent != "" {
					reasoningContent = true
				}
				if len(ch.Delta.ReasoningDetails) > 0 {
					details = true
				}
			}
		}
		return reasoning, reasoningContent, details
	}

	t.Run("both", func(t *testing.T) {
		r, rc, d := collect("both")
		if !r || !rc || !d {
			t.Fatalf("both: reasoning=%v reasoning_content=%v details=%v, want all true", r, rc, d)
		}
	})
	t.Run("reasoning only", func(t *testing.T) {
		r, rc, d := collect("reasoning")
		if !r || rc || !d {
			t.Fatalf("reasoning: reasoning=%v reasoning_content=%v details=%v, want true/false/true", r, rc, d)
		}
	})
	t.Run("reasoning_content only", func(t *testing.T) {
		r, rc, d := collect("reasoning_content")
		if r || !rc || !d {
			t.Fatalf("reasoning_content: reasoning=%v reasoning_content=%v details=%v, want false/true/true", r, rc, d)
		}
	})
	t.Run("off", func(t *testing.T) {
		r, rc, d := collect("off")
		if r || rc || d {
			t.Fatalf("off: reasoning=%v reasoning_content=%v details=%v, want all false", r, rc, d)
		}
	})
}

func parseChatStreamChunks(t *testing.T, body string) []openai.ChatCompletionChunk {
	t.Helper()
	var chunks []openai.ChatCompletionChunk
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if strings.TrimSpace(data) == "[DONE]" {
			continue
		}
		var chunk openai.ChatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("bad chat SSE frame %q: %v", data, err)
		}
		chunks = append(chunks, chunk)
	}
	return chunks
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

func TestResponsesNonStreamingReasoningEmissionOffSuppressesReasoning(t *testing.T) {
	gw := &reasoningResponseGateway{}
	s := New(config.Config{ReasoningEmission: "off"}, gw, slog.Default())
	body := strings.NewReader(`{"model":"gpt-5","input":"hi"}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if !gw.got.SuppressReasoning {
		t.Fatal("Responses gateway request should suppress reasoning when policy is off")
	}
	var resp openai.Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, item := range resp.Output {
		if item.Type == "reasoning" {
			t.Fatalf("non-streaming response should not include reasoning when policy is off: %#v", resp.Output)
		}
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
	events := parseResponseStreamEvents(t, w.Body.String())
	assertMonotonicSequence(t, events)

	types := eventTypes(events)
	// Full OpenAI reasoning-summary lifecycle, bracketed by the summary part, must
	// precede the message output item.
	assertOrderedSubsequence(t, types, []string{
		"response.output_item.added", // reasoning item
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.added", // message item
		"response.output_text.delta",
		"response.output_item.done", // reasoning item, enriched from terminal response
		"response.completed",
	})

	reasoningAdded := firstItemEvent(events, "response.output_item.added", "reasoning")
	if reasoningAdded == nil || reasoningAdded.OutputIndex == nil || *reasoningAdded.OutputIndex != 0 {
		t.Fatalf("reasoning item must be announced at output_index 0: %#v", reasoningAdded)
	}
	messageAdded := firstItemEvent(events, "response.output_item.added", "message")
	if messageAdded == nil || messageAdded.OutputIndex == nil || *messageAdded.OutputIndex != 1 {
		t.Fatalf("message item must shift to output_index 1 when reasoning present: %#v", messageAdded)
	}
	for _, e := range events {
		if e.Type == "response.reasoning_summary_text.delta" {
			if e.SummaryIndex == nil || *e.SummaryIndex != 0 || e.Delta == "" {
				t.Fatalf("summary text delta malformed: %#v", e)
			}
		}
	}
}

func TestResponsesStreamCarriesEncryptedContentOnStreamedReasoningDone(t *testing.T) {
	s := New(config.Config{}, &encryptedTextReasoningResponseStreamGateway{}, slog.Default())
	body := strings.NewReader(`{"model":"gpt-5","stream":true,"input":"hi"}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	events := parseResponseStreamEvents(t, w.Body.String())
	var done *openai.ResponseStreamEvent
	for i := range events {
		if events[i].Type == "response.output_item.done" && events[i].Item != nil && events[i].Item.Type == "reasoning" {
			done = &events[i]
			break
		}
	}
	if done == nil || done.Item.EncryptedContent != "enc-blob" {
		t.Fatalf("streamed reasoning done item lost encrypted_content: %#v", done)
	}
	if len(done.Item.Summary) != 1 || done.Item.Summary[0].Text != "thinking" {
		t.Fatalf("streamed reasoning done item lost summary: %#v", done.Item)
	}
}

func TestResponsesStreamReasoningEmissionOffSuppressesReasoning(t *testing.T) {
	s := New(config.Config{ReasoningEmission: "off"}, &reasoningResponseStreamGateway{}, slog.Default())
	body := strings.NewReader(`{"model":"claude-sonnet-4.6","stream":true,"input":"hi"}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	events := parseResponseStreamEvents(t, w.Body.String())
	for _, e := range events {
		if strings.Contains(e.Type, "reasoning") || (e.Item != nil && e.Item.Type == "reasoning") {
			t.Fatalf("reasoning event/item should be suppressed when policy is off: %#v", e)
		}
		if e.Response != nil {
			for _, item := range e.Response.Output {
				if item.Type == "reasoning" {
					t.Fatalf("completed response should not include reasoning when policy is off: %#v", e.Response.Output)
				}
			}
		}
	}
}

func TestResponsesStreamReconcilesEncryptedOnlyReasoning(t *testing.T) {
	s := New(config.Config{}, &encryptedReasoningResponseStreamGateway{}, slog.Default())
	body := strings.NewReader(`{"model":"gpt-5","stream":true,"input":"hi"}`)
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/responses", body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	events := parseResponseStreamEvents(t, w.Body.String())
	assertMonotonicSequence(t, events)

	// The reasoning item has no streamable summary, but it must still be announced
	// (added + done) so it is never an unannounced item in response.completed.
	reasoningAdded := firstItemEvent(events, "response.output_item.added", "reasoning")
	if reasoningAdded == nil || reasoningAdded.OutputIndex == nil || *reasoningAdded.OutputIndex != 0 {
		t.Fatalf("encrypted-only reasoning item must be reconciled at output_index 0: %#v", reasoningAdded)
	}
	if reasoningAdded.Item == nil || reasoningAdded.Item.EncryptedContent != "enc-blob" {
		t.Fatalf("reconciled reasoning item lost encrypted content: %#v", reasoningAdded.Item)
	}
	if firstItemEvent(events, "response.output_item.done", "reasoning") == nil {
		t.Fatalf("reconciled reasoning item was never closed:\n%s", w.Body.String())
	}
	// No summary text means no summary lifecycle events.
	for _, e := range events {
		if strings.HasPrefix(e.Type, "response.reasoning_summary") {
			t.Fatalf("unexpected summary event for encrypted-only reasoning: %#v", e)
		}
	}
	// The function call follows the reasoning item at output_index 1.
	fc := firstItemEvent(events, "response.output_item.added", "function_call")
	if fc == nil || fc.OutputIndex == nil || *fc.OutputIndex != 1 {
		t.Fatalf("function_call must be announced at output_index 1: %#v", fc)
	}
}

type encryptedTextReasoningResponseStreamGateway struct {
	copilotgw.Gateway
}

func (g *encryptedTextReasoningResponseStreamGateway) StreamResponse(_ context.Context, req copilotgw.ResponseRequest) (<-chan copilotgw.ResponseStreamEvent, error) {
	ch := make(chan copilotgw.ResponseStreamEvent, 8)
	go func() {
		defer close(ch)
		ch <- copilotgw.ResponseStreamEvent{Kind: "reasoning_delta", Delta: "thinking", ReasoningID: "rid-1"}
		ch <- copilotgw.ResponseStreamEvent{Kind: "delta", Delta: "answer"}
		resp := responseForReasoningTest(req, &copilotgw.TurnResult{Text: "answer", Reasoning: "thinking", ReasoningID: "rid-1"})
		resp.Output[0].EncryptedContent = "enc-blob"
		ch <- copilotgw.ResponseStreamEvent{Kind: "response", Response: resp}
	}()
	return ch, nil
}

type encryptedReasoningResponseStreamGateway struct {
	copilotgw.Gateway
}

func (g *encryptedReasoningResponseStreamGateway) StreamResponse(_ context.Context, req copilotgw.ResponseRequest) (<-chan copilotgw.ResponseStreamEvent, error) {
	ch := make(chan copilotgw.ResponseStreamEvent, 2)
	go func() {
		defer close(ch)
		// A tool-only turn whose reasoning is encrypted-only: no reasoning_delta
		// and no content delta are streamed, so the reasoning item only appears in
		// the terminal response and must be reconciled by the encoder.
		resp := &openai.Response{
			ID:        req.ResponseID,
			Object:    openai.ObjectResponse,
			CreatedAt: openai.UnixNow(),
			Status:    "completed",
			Model:     req.Model,
			Output: []openai.ResponseOutputItem{
				{ID: "rs_rid-1", Type: "reasoning", Status: "completed", EncryptedContent: "enc-blob", Summary: []openai.ResponseReasoningSummary{}},
				{ID: "fc_call_1", Type: "function_call", Status: "completed", CallID: "call_1", Name: "lookup", Arguments: `{"q":"x"}`},
			},
			ParallelToolCalls: true,
			Store:             req.Store,
		}
		ch <- copilotgw.ResponseStreamEvent{Kind: "response", Response: resp}
	}()
	return ch, nil
}

// --- SSE parsing helpers (assert structured events, not raw substrings) ---

func parseResponseStreamEvents(t *testing.T, body string) []openai.ResponseStreamEvent {
	t.Helper()
	var events []openai.ResponseStreamEvent
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if strings.TrimSpace(data) == "[DONE]" {
			continue
		}
		var ev openai.ResponseStreamEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			t.Fatalf("bad SSE data frame %q: %v", data, err)
		}
		events = append(events, ev)
	}
	return events
}

func eventTypes(events []openai.ResponseStreamEvent) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	return types
}

func assertOrderedSubsequence(t *testing.T, got, want []string) {
	t.Helper()
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("missing ordered events; matched %d/%d of %v\nactual: %v", i, len(want), want, got)
	}
}

func assertMonotonicSequence(t *testing.T, events []openai.ResponseStreamEvent) {
	t.Helper()
	prev := int64(-1)
	for _, e := range events {
		if e.SequenceNumber <= prev {
			t.Fatalf("sequence_number not strictly increasing: %d after %d (%s)", e.SequenceNumber, prev, e.Type)
		}
		prev = e.SequenceNumber
	}
}

func firstItemEvent(events []openai.ResponseStreamEvent, eventType, itemType string) *openai.ResponseStreamEvent {
	for i := range events {
		e := events[i]
		if e.Type == eventType && e.Item != nil && e.Item.Type == itemType {
			return &events[i]
		}
	}
	return nil
}
