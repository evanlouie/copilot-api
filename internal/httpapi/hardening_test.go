package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/evanlouie/copilot-api/internal/config"
	"github.com/evanlouie/copilot-api/internal/copilotgw"
	"github.com/evanlouie/copilot-api/internal/openai"
)

func TestDecodeJSONAcceptsExactLimitAndRejectsOneByteOver(t *testing.T) {
	payload := `{"ok":1}`
	for _, test := range []struct {
		name   string
		limit  int64
		status int
	}{{"exact", int64(len(payload)), 0}, {"over", int64(len(payload) - 1), http.StatusRequestEntityTooLarge}} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(payload))
			response := httptest.NewRecorder()
			var decoded map[string]any
			err := decodeJSON(response, request, test.limit, &decoded)
			if test.status == 0 && err != nil {
				t.Fatal(err)
			}
			if test.status != 0 {
				var apiError *openai.APIError
				if !errors.As(err, &apiError) || apiError.Status != test.status {
					t.Fatalf("error = %#v, want status %d", err, test.status)
				}
			}
		})
	}
}

func TestOversizedJSONBodyReturns413(t *testing.T) {
	server := New(config.Config{MaxRequestBodyBytes: 32}, &captureChatGateway{}, slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"too large"}]}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"code":"request_too_large"`) {
		t.Fatalf("body = %s, want request_too_large", response.Body.String())
	}
}

func TestChatRejectsParallelToolCallsFalse(t *testing.T) {
	server := New(config.Config{}, &captureChatGateway{}, slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","parallel_tool_calls":false,"messages":[{"role":"user","content":"hi"}]}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"param":"parallel_tool_calls"`) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
}

type scriptedChatStreamGateway struct {
	copilotgw.Gateway
	events []copilotgw.StreamEvent
	hold   bool
	err    error
}

func (g *scriptedChatStreamGateway) StreamChat(ctx context.Context, _ copilotgw.ChatRequest) (<-chan copilotgw.StreamEvent, error) {
	if g.err != nil {
		return nil, g.err
	}
	channel := make(chan copilotgw.StreamEvent, len(g.events))
	for _, event := range g.events {
		channel <- event
	}
	if !g.hold {
		close(channel)
	} else {
		go func() {
			<-ctx.Done()
			close(channel)
		}()
	}
	return channel, nil
}

func TestChatSynchronousStreamSetupFailurePreservesHTTPStatus(t *testing.T) {
	server := New(config.Config{}, &scriptedChatStreamGateway{err: openai.NotFound("missing model", "model_not_found")}, slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"missing","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("response = %d %s headers=%v", response.Code, response.Body.String(), response.Header())
	}
}

func TestChatTerminalOnlyTextIsEmittedBeforeFinish(t *testing.T) {
	result := &copilotgw.TurnResult{ID: "chatcmpl_terminal", Model: "gpt-5", Text: "terminal text", FinishReason: "stop"}
	server := New(config.Config{}, &scriptedChatStreamGateway{events: []copilotgw.StreamEvent{{Kind: "result", Result: result}}}, slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	body := response.Body.String()
	if !strings.Contains(body, `"content":"terminal text"`) || strings.Index(body, `"content":"terminal text"`) > strings.Index(body, `"finish_reason":"stop"`) || strings.Count(body, "data: [DONE]") != 1 {
		t.Fatalf("body = %s", body)
	}
}

func TestChatTerminalReasoningSuffixIsEmitted(t *testing.T) {
	result := &copilotgw.TurnResult{ID: "chatcmpl_reason", Model: "gpt-5", Reasoning: "thinking", FinishReason: "stop"}
	server := New(config.Config{}, &scriptedChatStreamGateway{events: []copilotgw.StreamEvent{{Kind: "reasoning_delta", Delta: "think"}, {Kind: "result", Result: result}}}, slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if strings.Count(response.Body.String(), `"reasoning":"think"`) != 1 || strings.Count(response.Body.String(), `"reasoning":"ing"`) != 1 {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestChatStreamPrematureClosureEmitsErrorAndDone(t *testing.T) {
	server := New(config.Config{}, &scriptedChatStreamGateway{}, slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), `"code":"upstream_error"`) || !strings.Contains(response.Body.String(), "data: [DONE]") {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestChatStreamStopsAfterError(t *testing.T) {
	result := &copilotgw.TurnResult{ID: "chatcmpl_late", Model: "gpt-5", FinishReason: "stop"}
	gateway := &scriptedChatStreamGateway{events: []copilotgw.StreamEvent{{Kind: "error", Error: openai.Upstream("boom")}, {Kind: "result", Result: result}}}
	server := New(config.Config{}, gateway, slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if strings.Contains(response.Body.String(), `"finish_reason":"stop"`) || strings.Contains(response.Body.String(), "chatcmpl_late") || strings.Count(response.Body.String(), "data: [DONE]") != 1 {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestChatStreamDeadlineEmitsTimeout(t *testing.T) {
	server := New(config.Config{RequestTimeout: 20 * time.Millisecond}, &scriptedChatStreamGateway{hold: true}, slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), `"code":"request_timeout"`) || !strings.Contains(response.Body.String(), "data: [DONE]") {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestChatStreamClientCancellationDoesNotWriteTerminal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	cancel()
	response := httptest.NewRecorder()
	server := New(config.Config{}, &scriptedChatStreamGateway{hold: true}, slog.Default())
	server.Handler().ServeHTTP(response, request)
	if strings.Contains(response.Body.String(), "data: [DONE]") || strings.Contains(response.Body.String(), `"error"`) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

func TestSynchronousStreamSetupFailurePreservesHTTPStatus(t *testing.T) {
	server := New(config.Config{}, &errorResponseGateway{err: openai.NotFound("missing model", "model_not_found")}, slog.Default())
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"missing","stream":true,"input":"hi"}`))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("response = %d %s headers=%v", response.Code, response.Body.String(), response.Header())
	}
}

func TestPrematureResponseStreamClosureEmitsFailure(t *testing.T) {
	channel := make(chan copilotgw.ResponseStreamEvent)
	close(channel)
	writer := &captureResponseEventWriter{}
	result := writeResponseStreamEvents(context.Background(), writer, copilotgw.ResponseRequest{ResponseID: "resp_closed", Model: "gpt-5"}, 0, channel)
	if result.Err == nil || result.WriteFailed || !result.FailureWritten {
		t.Fatalf("result = %#v, want emitted upstream failure", result)
	}
	last := writer.events[len(writer.events)-1]
	if last.Type != "response.failed" || last.Error == nil || last.Error.Code != "upstream_error" {
		t.Fatalf("terminal event = %#v", last)
	}
}

func TestResponseStreamReconcilesTerminalReasoningAfterSummaryDone(t *testing.T) {
	response := &openai.Response{ID: "resp_reason_suffix", Object: openai.ObjectResponse, Status: "completed", Model: "gpt-5", Output: []openai.ResponseOutputItem{{ID: "rs_suffix", Type: "reasoning", Status: "completed", Summary: []openai.ResponseReasoningSummary{{Type: "summary_text", Text: "thinking"}}}}}
	channel := make(chan copilotgw.ResponseStreamEvent, 3)
	channel <- copilotgw.ResponseStreamEvent{Kind: "reasoning_delta", Delta: "think", ReasoningID: "suffix"}
	channel <- copilotgw.ResponseStreamEvent{Kind: "delta", Delta: "answer"}
	channel <- copilotgw.ResponseStreamEvent{Kind: "response", Response: response}
	close(channel)
	writer := &captureResponseEventWriter{}
	result := writeResponseStreamEvents(context.Background(), writer, copilotgw.ResponseRequest{ResponseID: response.ID, Model: response.Model}, 0, channel)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	var reasoning strings.Builder
	for _, event := range writer.events {
		if event.Type == "response.reasoning_summary_text.delta" {
			reasoning.WriteString(event.Delta)
		}
	}
	if reasoning.String() != "thinking" || len(result.Response.Output[0].Summary) != 2 {
		t.Fatalf("reasoning=%q response=%#v", reasoning.String(), result.Response)
	}
}

func TestResponseStreamDeadlineClosesPartialItemsAndUsesTimeoutError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	channel := make(chan copilotgw.ResponseStreamEvent, 1)
	channel <- copilotgw.ResponseStreamEvent{Kind: "delta", Delta: "partial"}
	writer := &captureResponseEventWriter{}
	result := writeResponseStreamEvents(ctx, writer, copilotgw.ResponseRequest{ResponseID: "resp_timeout", Model: "gpt-5"}, 1024, channel)
	if result.Err == nil || result.WriteFailed || !result.FailureWritten {
		t.Fatalf("result = %#v, want emitted timeout failure", result)
	}
	types := strings.Join(writer.types(), ",")
	for _, required := range []string{"response.output_text.done", "response.content_part.done", "response.output_item.done", "response.failed"} {
		if !strings.Contains(types, required) {
			t.Fatalf("events %s missing %s", types, required)
		}
	}
	last := writer.events[len(writer.events)-1]
	if last.Error == nil || last.Error.Code != "request_timeout" {
		t.Fatalf("terminal error = %#v", last.Error)
	}
	if last.Response == nil || len(last.Response.Output) != 1 || last.Response.Output[0].Status != "incomplete" || last.Response.OutputText != "partial" {
		t.Fatalf("failed response lost partial output: %#v", last.Response)
	}
}

func TestResponseStreamClientCancellationDoesNotAttemptTerminalFrame(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writer := &captureResponseEventWriter{}
	result := writeResponseStreamEvents(ctx, writer, copilotgw.ResponseRequest{ResponseID: "resp_cancel", Model: "gpt-5"}, 0, make(chan copilotgw.ResponseStreamEvent))
	if !errors.Is(result.Err, context.Canceled) || !result.WriteFailed {
		t.Fatalf("result = %#v", result)
	}
	for _, event := range writer.events {
		if event.Type == "response.failed" || event.Type == "response.completed" {
			t.Fatalf("unexpected terminal frame after client cancellation: %#v", event)
		}
	}
}

func TestResponseStreamStopsAfterCompletedResponse(t *testing.T) {
	response := &openai.Response{ID: "resp_done", Object: openai.ObjectResponse, Status: "completed", Model: "gpt-5", Output: []openai.ResponseOutputItem{}}
	channel := make(chan copilotgw.ResponseStreamEvent, 2)
	channel <- copilotgw.ResponseStreamEvent{Kind: "response", Response: response}
	channel <- copilotgw.ResponseStreamEvent{Kind: "error", Error: openai.Upstream("late")}
	close(channel)
	writer := &captureResponseEventWriter{}
	result := writeResponseStreamEvents(context.Background(), writer, copilotgw.ResponseRequest{ResponseID: response.ID, Model: response.Model}, 0, channel)
	if result.Err != nil || result.Response != response {
		t.Fatalf("result = %#v", result)
	}
	for _, event := range writer.events {
		if event.Type == "response.failed" {
			t.Fatalf("late failure was emitted: %#v", writer.events)
		}
	}
}

func TestResponseStreamEmitsTerminalTextSuffix(t *testing.T) {
	response := &openai.Response{ID: "resp_suffix", Object: openai.ObjectResponse, Status: "completed", Model: "gpt-5", OutputText: "partial", Output: []openai.ResponseOutputItem{{ID: "msg_suffix", Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: "partial"}}}}}
	channel := make(chan copilotgw.ResponseStreamEvent, 2)
	channel <- copilotgw.ResponseStreamEvent{Kind: "delta", Delta: "part"}
	channel <- copilotgw.ResponseStreamEvent{Kind: "response", Response: response}
	close(channel)
	writer := &captureResponseEventWriter{}
	result := writeResponseStreamEvents(context.Background(), writer, copilotgw.ResponseRequest{ResponseID: response.ID, Model: response.Model}, 0, channel)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	var deltas strings.Builder
	for _, event := range writer.events {
		if event.Type == "response.output_text.delta" {
			deltas.WriteString(event.Delta)
		}
	}
	if deltas.String() != "partial" {
		t.Fatalf("streamed text = %q", deltas.String())
	}
}

func TestResponseOutputSizeIncludesRawArgumentsAndAnnotations(t *testing.T) {
	response := &openai.Response{Output: []openai.ResponseOutputItem{
		{Type: "tool_search_call", ArgumentsJSON: []byte(`{"query":"large"}`)},
		{Type: "message", Content: []openai.ResponseText{{Type: "output_text", Annotations: []any{map[string]any{"url": "https://example.com"}}}}},
	}}
	size, err := responseOutputPayloadBytes(response)
	if err != nil {
		t.Fatal(err)
	}
	if size < int64(len(response.Output[0].ArgumentsJSON)+len("https://example.com")) {
		t.Fatalf("payload size = %d", size)
	}
}

func TestTerminalOnlyResponseHonorsStreamOutputLimit(t *testing.T) {
	response := &openai.Response{ID: "resp_large_terminal", Object: openai.ObjectResponse, Status: "completed", Model: "gpt-5", OutputText: "oversized", Output: []openai.ResponseOutputItem{{ID: "msg_large", Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: "oversized"}}}}}
	channel := make(chan copilotgw.ResponseStreamEvent, 1)
	channel <- copilotgw.ResponseStreamEvent{Kind: "response", Response: response}
	close(channel)
	writer := &captureResponseEventWriter{}
	result := writeResponseStreamEvents(context.Background(), writer, copilotgw.ResponseRequest{ResponseID: response.ID, Model: response.Model}, 4, channel)
	if result.Err == nil || !result.FailureWritten || writer.events[len(writer.events)-1].Type != "response.failed" {
		t.Fatalf("result=%#v events=%#v", result, writer.events)
	}
}

func TestTerminalOnlyResponseMessageHasCompleteLifecycle(t *testing.T) {
	response := &openai.Response{ID: "resp_terminal", Object: openai.ObjectResponse, Status: "completed", Model: "gpt-5", OutputText: "terminal text", Output: []openai.ResponseOutputItem{{ID: "msg_terminal", Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: "terminal text"}}}}}
	channel := make(chan copilotgw.ResponseStreamEvent, 1)
	channel <- copilotgw.ResponseStreamEvent{Kind: "response", Response: response}
	close(channel)
	writer := &captureResponseEventWriter{}
	result := writeResponseStreamEvents(context.Background(), writer, copilotgw.ResponseRequest{ResponseID: response.ID, Model: response.Model}, 0, channel)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	types := strings.Join(writer.types(), ",")
	for _, required := range []string{"response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.content_part.done", "response.output_item.done", "response.completed"} {
		if !strings.Contains(types, required) {
			t.Fatalf("events %s missing %s", types, required)
		}
	}
	if writer.events[len(writer.events)-1].Type != "response.completed" {
		t.Fatalf("last event = %#v", writer.events[len(writer.events)-1])
	}
}

func TestContentFirstTerminalReasoningHasCompleteLifecycle(t *testing.T) {
	channel := make(chan copilotgw.ResponseStreamEvent, 2)
	channel <- copilotgw.ResponseStreamEvent{Kind: "delta", Delta: "answer"}
	channel <- copilotgw.ResponseStreamEvent{Kind: "response", Response: &openai.Response{
		ID: "resp_reasoning", Object: openai.ObjectResponse, Status: "completed", Model: "gpt-5", OutputText: "answer",
		Output: []openai.ResponseOutputItem{
			{ID: "rs_1", Type: "reasoning", Status: "completed", EncryptedContent: "encrypted"},
			{ID: "msg_1", Type: "message", Status: "completed", Role: "assistant", Content: []openai.ResponseText{{Type: "output_text", Text: "answer"}}},
		},
	}}
	close(channel)
	writer := &captureResponseEventWriter{}
	result := writeResponseStreamEvents(context.Background(), writer, copilotgw.ResponseRequest{ResponseID: "resp_reasoning", Model: "gpt-5"}, 0, channel)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	var reasoningAdded, reasoningDone *openai.ResponseStreamEvent
	for i := range writer.events {
		event := &writer.events[i]
		if event.Item != nil && event.Item.Type == "reasoning" {
			switch event.Type {
			case "response.output_item.added":
				reasoningAdded = event
			case "response.output_item.done":
				reasoningDone = event
			}
		}
	}
	if reasoningAdded == nil || reasoningDone == nil || reasoningAdded.OutputIndex == nil || *reasoningAdded.OutputIndex != 1 {
		t.Fatalf("reasoning lifecycle incomplete: added=%#v done=%#v", reasoningAdded, reasoningDone)
	}
	if result.Response == nil || len(result.Response.Output) != 2 || result.Response.Output[0].Type != "message" || result.Response.Output[1].Type != "reasoning" {
		t.Fatalf("completed output order = %#v", result.Response)
	}
}

func TestServerShutdownClosesActiveWebSockets(t *testing.T) {
	server := New(config.Config{}, &websocketStreamGateway{}, slog.Default())
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	dialCtx, cancelDial := context.WithTimeout(context.Background(), time.Second)
	conn, _, err := websocket.Dial(dialCtx, "ws"+strings.TrimPrefix(httpServer.URL, "http")+"/v1/responses", nil)
	cancelDial()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	readResult := make(chan error, 1)
	go func() {
		readCtx, cancelRead := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelRead()
		var value any
		readResult <- wsjson.Read(readCtx, conn, &value)
	}()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		t.Fatal(err)
	}
	err = <-readResult
	if err == nil {
		t.Fatal("websocket remained open after shutdown")
	}
	if status := websocket.CloseStatus(err); status != websocket.StatusGoingAway && status != websocket.StatusNormalClosure {
		t.Fatalf("close status = %v, error=%v", status, err)
	}
}

func TestAuthenticationFailureSamplerBoundsRepeatedLogs(t *testing.T) {
	sampler := newFailureLogSampler(time.Minute)
	now := time.Now()
	if !sampler.Allow("127.0.0.1", now) {
		t.Fatal("first failure was suppressed")
	}
	for i := 0; i < 200; i++ {
		if sampler.Allow("127.0.0.1", now) {
			t.Fatalf("failure %d unexpectedly logged", i+2)
		}
	}
	if !sampler.Allow("127.0.0.1", now.Add(time.Minute)) {
		t.Fatal("first failure in the next window was suppressed")
	}
}

func TestUnknownWebSocketErrorsAreGeneric(t *testing.T) {
	event := openai.NewWebSocketErrorEvent(errors.New("/secret/path"), "evt")
	if strings.Contains(event.Error.Message, "/secret/path") || event.Error.Message != "internal server error" {
		t.Fatalf("websocket error leaked details: %#v", event)
	}
}
