package openai

import (
	"encoding/json"
	"testing"
)

func TestResolveReasoningEmission(t *testing.T) {
	tests := []struct {
		policy               string
		emitReasoning        bool
		emitReasoningContent bool
	}{
		{"both", true, true},
		{"", true, true},
		{"unknown", true, true},
		{"reasoning", true, false},
		{"reasoning_content", false, true},
		{"off", false, false},
	}
	for _, tt := range tests {
		got := ResolveReasoningEmission(tt.policy)
		if got.EmitReasoning != tt.emitReasoning || got.EmitReasoningContent != tt.emitReasoningContent {
			t.Fatalf("ResolveReasoningEmission(%q) = %#v, want reasoning=%v content=%v", tt.policy, got, tt.emitReasoning, tt.emitReasoningContent)
		}
		if got.Enabled() != (tt.emitReasoning || tt.emitReasoningContent) {
			t.Fatalf("Enabled(%q) = %v", tt.policy, got.Enabled())
		}
	}
}

func TestBuildReasoningDetailsAnthropicSignedAndEncrypted(t *testing.T) {
	details := BuildReasoningDetails("thinking", "sig-blob", "enc-blob", "rid-1")
	if len(details) != 2 {
		t.Fatalf("details length = %d, want 2: %#v", len(details), details)
	}
	text := details[0]
	if text.Type != "reasoning.text" || text.Text != "thinking" || text.Signature != "sig-blob" || text.Format != "anthropic-claude-v1" || text.ID != "rid-1" {
		t.Fatalf("text detail = %#v", text)
	}
	enc := details[1]
	if enc.Type != "reasoning.encrypted" || enc.Data != "enc-blob" || enc.ID != "rid-1" {
		t.Fatalf("encrypted detail = %#v", enc)
	}
}

func TestBuildReasoningDetailsPlaintextOnlyHasNoSignatureOrFormat(t *testing.T) {
	details := BuildReasoningDetails("thinking", "", "", "")
	if len(details) != 1 {
		t.Fatalf("details length = %d, want 1: %#v", len(details), details)
	}
	b, err := json.Marshal(details[0])
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if got != `{"type":"reasoning.text","text":"thinking"}` {
		t.Fatalf("plaintext detail JSON = %s, want no signature/format/id keys", got)
	}
}

func TestBuildReasoningDetailsEmptyReturnsNil(t *testing.T) {
	if details := BuildReasoningDetails("", "", "", "rid"); details != nil {
		t.Fatalf("expected nil details for empty reasoning, got %#v", details)
	}
}

func TestChatMessageToleratesInboundReasoning(t *testing.T) {
	var msg ChatMessage
	body := []byte(`{"role":"assistant","content":"hello","reasoning":"because","reasoning_content":"because","reasoning_details":[{"type":"reasoning.text","text":"because","signature":"sig","format":"anthropic-claude-v1","index":0}]}`)
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("inbound reasoning must not error: %v", err)
	}
	if msg.Reasoning != "because" || msg.ReasoningContent != "because" {
		t.Fatalf("reasoning fields = %q / %q", msg.Reasoning, msg.ReasoningContent)
	}
	if len(msg.ReasoningDetails) != 1 || msg.ReasoningDetails[0].Signature != "sig" || msg.ReasoningDetails[0].Index == nil || *msg.ReasoningDetails[0].Index != 0 {
		t.Fatalf("reasoning_details = %#v", msg.ReasoningDetails)
	}
}

func TestValidateChatAcceptsParallelToolCalls(t *testing.T) {
	for _, strict := range []bool{false, true} {
		var req ChatCompletionRequest
		body := []byte(`{"model":"gpt-5","parallel_tool_calls":true,"messages":[{"role":"user","content":"hi"}]}`)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		if err := ValidateChatRequest(&req, strict); err != nil {
			t.Fatalf("parallel_tool_calls=true should be accepted (strict=%v): %v", strict, err)
		}
	}
}
