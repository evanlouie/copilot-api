package openai

import "strings"

// Reasoning emission policy: which reasoning fields a surface puts on the wire,
// and how the structured reasoning_details array is assembled. This is business
// policy rather than a wire DTO, so it is kept apart from the request/response
// types it decorates.

// InboundReasoning returns client-supplied assistant reasoning so it can be
// replayed when rebuilding a cold session. It prefers the canonical `reasoning`
// alias, then `reasoning_content`, then concatenated textual/summary
// `reasoning_details` blocks (the OpenRouter round-trip shape).
// Opaque/encrypted reasoning is session-bound and intentionally not replayed.
func (m ChatMessage) InboundReasoning() string {
	if m.Reasoning != "" {
		return m.Reasoning
	}
	if m.ReasoningContent != "" {
		return m.ReasoningContent
	}
	parts := make([]string, 0, len(m.ReasoningDetails))
	for _, d := range m.ReasoningDetails {
		switch {
		case d.Text != "":
			parts = append(parts, d.Text)
		case d.Summary != "":
			parts = append(parts, d.Summary)
		}
	}
	return strings.Join(parts, "")
}

// ReasoningEmissionPolicy resolves which reasoning fields a surface should
// emit. reasoning_details is always emitted when the policy is enabled,
// regardless of which plaintext alias is selected.
type ReasoningEmissionPolicy struct {
	EmitReasoning        bool
	EmitReasoningContent bool
}

func (p ReasoningEmissionPolicy) Enabled() bool {
	return p.EmitReasoning || p.EmitReasoningContent
}

// ResolveReasoningEmission maps a config policy string to the concrete fields
// to emit. Unknown/empty values default to the max-compatibility "both".
func ResolveReasoningEmission(policy string) ReasoningEmissionPolicy {
	switch policy {
	case "off":
		return ReasoningEmissionPolicy{}
	case "reasoning":
		return ReasoningEmissionPolicy{EmitReasoning: true}
	case "reasoning_content":
		return ReasoningEmissionPolicy{EmitReasoningContent: true}
	default:
		return ReasoningEmissionPolicy{EmitReasoning: true, EmitReasoningContent: true}
	}
}

// BuildReasoningDetails assembles the structured reasoning_details array from
// the plaintext thinking, the Anthropic signed/opaque blob, and the OpenAI
// encrypted blob. The signed text and encrypted payloads are preserved
// byte-for-byte so clients can replay them for continuity.
func BuildReasoningDetails(text, signature, encrypted, id string) []ReasoningDetail {
	var details []ReasoningDetail
	if text != "" || signature != "" {
		detail := ReasoningDetail{Type: "reasoning.text", Text: text, ID: id}
		if signature != "" {
			detail.Signature = signature
			detail.Format = "anthropic-claude-v1"
		}
		details = append(details, detail)
	}
	if encrypted != "" {
		details = append(details, ReasoningDetail{Type: "reasoning.encrypted", Data: encrypted, ID: id})
	}
	return details
}
