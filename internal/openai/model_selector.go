package openai

import "strings"

// ModelSelector is the OpenAI-facing model selector split into its canonical
// model ID and optional explicit reasoning effort.
type ModelSelector struct {
	Model           string
	ReasoningEffort string
	HasEffort       bool
}

// ParseModelSelector parses <model>[:<reasoning-effort>] using the final colon
// as the separator. The model ID is preserved exactly; the optional effort is
// trimmed and normalized to lowercase.
func ParseModelSelector(raw string) (ModelSelector, error) {
	separator := strings.LastIndex(raw, ":")
	if separator < 0 {
		if raw == "" {
			return ModelSelector{}, InvalidRequest("model is required", "model")
		}
		return ModelSelector{Model: raw}, nil
	}

	model := raw[:separator]
	if model == "" {
		return ModelSelector{}, InvalidRequest("model selector must include a model before the reasoning effort suffix", "model")
	}
	effort := NormalizeReasoningEffort(raw[separator+1:])
	if effort == "" {
		return ModelSelector{}, InvalidRequest("model reasoning effort suffix must not be empty", "model")
	}
	return ModelSelector{Model: model, ReasoningEffort: effort, HasEffort: true}, nil
}

// NormalizeReasoningEffort applies the normalization used for explicit request
// efforts throughout the OpenAI compatibility boundary.
func NormalizeReasoningEffort(effort string) string {
	return strings.ToLower(strings.TrimSpace(effort))
}

// MergeReasoningEffort combines a model suffix with another explicit request
// spelling. Matching normalized values are accepted; contradictory values are
// rejected rather than silently applying precedence.
func MergeReasoningEffort(selector ModelSelector, explicit, explicitParam string) (string, error) {
	normalized := NormalizeReasoningEffort(explicit)
	if !selector.HasEffort {
		return normalized, nil
	}
	if normalized != "" && normalized != selector.ReasoningEffort {
		if explicitParam == "" {
			explicitParam = "reasoning_effort"
		}
		return "", InvalidRequest("model reasoning effort suffix conflicts with "+explicitParam, explicitParam)
	}
	return selector.ReasoningEffort, nil
}
