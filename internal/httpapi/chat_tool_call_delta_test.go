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

// streamChatToolFragmentGateway plans one tool call whose arguments arrive as
// fragments, then finishes the turn with the assembled call.
type streamChatToolFragmentGateway struct {
	unimplementedGateway
	fragments    []string
	arguments    string
	dropToolCall bool
}

func (g *streamChatToolFragmentGateway) StreamChat(_ context.Context, req copilotgw.ChatRequest) (<-chan copilotgw.StreamEvent, error) {
	ch := make(chan copilotgw.StreamEvent, len(g.fragments)+1)
	go func() {
		defer close(ch)
		for _, fragment := range g.fragments {
			ch <- copilotgw.StreamEvent{Kind: "tool_call_delta", ToolCallID: "call_1", ToolName: "lookup", Delta: fragment}
		}
		result := &copilotgw.TurnResult{
			ID:           req.OpenAIID,
			Created:      openai.UnixNow(),
			Model:        req.Model,
			FinishReason: "tool_calls",
			ToolCalls: []openai.ChatToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: openai.ToolCallFunction{Name: "lookup", Arguments: g.arguments},
			}},
		}
		// A turn that streamed fragments and then produced no matching call is the
		// divergence the terminal reconciliation exists to catch.
		if g.dropToolCall {
			result.FinishReason = "stop"
			result.ToolCalls = nil
		}
		ch <- copilotgw.StreamEvent{Kind: "result", Result: result}
	}()
	return ch, nil
}

const chatToolStreamBody = `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"look up alpha"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`

// A client that renders tool arguments as they arrive gets them fragment by
// fragment, in OpenAI's shape: a stable index, the identifying fields once, and
// nothing repeated at the end. The last part is the one that matters most -
// every client accumulates `function.arguments` across chunks, so a terminal
// chunk repeating them would double the call.
func TestChatStreamForwardsToolCallArgumentFragments(t *testing.T) {
	t.Parallel()
	gw := &streamChatToolFragmentGateway{fragments: []string{`{"q":`, `"al`, `pha"}`}, arguments: `{"q":"alpha"}`}
	s := New(config.Config{}, gw, slog.Default())
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatToolStreamBody)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusOK, w.Body.String())
	}
	deltas := chatToolCallDeltas(t, w.Body.String())
	if len(deltas) != len(gw.fragments) {
		t.Fatalf("streamed %d tool-call deltas, want one per fragment and none at the end:\n%s", len(deltas), w.Body.String())
	}
	var assembled strings.Builder
	for i, delta := range deltas {
		if delta.Index != 0 {
			t.Fatalf("tool-call delta[%d] index = %d, want a stable 0", i, delta.Index)
		}
		if i == 0 {
			if delta.ID != "call_1" || delta.Type != "function" || delta.Function.Name != "lookup" {
				t.Fatalf("first tool-call delta = %#v, want the id, type and name", delta)
			}
		} else if delta.ID != "" || delta.Type != "" || delta.Function.Name != "" {
			t.Fatalf("tool-call delta[%d] = %#v, want arguments only after the first fragment", i, delta)
		}
		assembled.WriteString(delta.Function.Arguments)
	}
	if assembled.String() != gw.arguments {
		t.Fatalf("accumulated arguments = %q, want %q", assembled.String(), gw.arguments)
	}
	if !strings.Contains(w.Body.String(), `"finish_reason":"tool_calls"`) {
		t.Fatalf("stream is missing its terminal finish chunk:\n%s", w.Body.String())
	}
}

// A turn whose tool call never streamed - a strict tool, or a backend that
// emits no fragments - must still deliver the whole call in one terminal chunk,
// byte for byte as it always has.
func TestChatStreamStillEmitsWholeToolCallWhenNothingWasStreamed(t *testing.T) {
	t.Parallel()
	gw := &streamChatToolFragmentGateway{arguments: `{"q":"alpha"}`}
	s := New(config.Config{}, gw, slog.Default())
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatToolStreamBody)))

	want := `"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"alpha\"}"}}]`
	if !strings.Contains(w.Body.String(), want) {
		t.Fatalf("stream missing %s:\n%s", want, w.Body.String())
	}
}

// Fragments that are not a prefix of the finished call cannot be repaired by
// the client, which has already accumulated them, so the stream fails rather
// than delivering arguments that disagree with themselves.
func TestChatStreamFailsWhenToolCallFragmentsDoNotReconcile(t *testing.T) {
	t.Parallel()
	gw := &streamChatToolFragmentGateway{fragments: []string{`{"q":"beta"}`}, arguments: `{"q":"alpha"}`}
	s := New(config.Config{}, gw, slog.Default())
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatToolStreamBody)))

	body := w.Body.String()
	if !strings.Contains(body, "do not match the streamed arguments") {
		t.Fatalf("stream did not report the divergence:\n%s", body)
	}
	if strings.Contains(body, `"finish_reason":"tool_calls"`) {
		t.Fatalf("stream finished normally despite the divergence:\n%s", body)
	}
}

// A call the client accumulated fragments for and never receives is a call it
// cannot answer, so the stream fails rather than leaving a dangling index.
func TestChatStreamFailsWhenAStreamedToolCallNeverArrives(t *testing.T) {
	t.Parallel()
	gw := &streamChatToolFragmentGateway{fragments: []string{`{"q":"alpha"}`}, dropToolCall: true}
	s := New(config.Config{}, gw, slog.Default())
	w := httptest.NewRecorder()

	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatToolStreamBody)))

	if body := w.Body.String(); !strings.Contains(body, "whose arguments were streamed") {
		t.Fatalf("stream did not report the missing tool call:\n%s", body)
	}
}

// chatToolCallDeltas collects every tool-call delta an SSE chat stream carried,
// in order.
func chatToolCallDeltas(t *testing.T, body string) []openai.ToolCallDelta {
	t.Helper()
	var deltas []openai.ToolCallDelta
	for _, frame := range strings.Split(body, "\n\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(frame), "data: ")
		if !ok || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []openai.ToolCallDelta `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("decode chat chunk %q: %v", payload, err)
		}
		for _, choice := range chunk.Choices {
			deltas = append(deltas, choice.Delta.ToolCalls...)
		}
	}
	return deltas
}
