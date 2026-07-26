package openai

import (
	"strings"

	"github.com/evanlouie/copilot-api/internal/apierr"
)

// ModelSelector is the OpenAI-facing model selector split into its canonical
// model ID and optional explicit reasoning effort.
type ModelSelector struct {
	Model           string
	ReasoningEffort string
	HasEffort       bool
}

// ParseModelSelector parses <model>[:<reasoning-effort>].
//
// A trailing `:suffix` is a reasoning effort only when the suffix names a
// canonical effort (see KnownReasoningEfforts). A colon is otherwise legal
// inside a model ID — "openrouter/mistral-7b:free" is one name, not a model
// plus an effort — so any other suffix stays on the model ID and the model
// catalog decides whether it exists. Splitting on the final colon
// unconditionally truncated such IDs and answered "model not found" for a name
// the client never sent, and forwarded "gpt-5:banana"'s "banana" to the SDK as
// an effort.
//
// Resolution order for the ambiguous case: if a real model were ever named with
// a canonical effort as its final segment (a hypothetical "foo:low"), the effort
// reading wins and that model is unreachable. Parsing runs before any catalog
// lookup — it is synchronous while the catalog is a network round trip, and the
// suffix is the documented meaning of a trailing colon on this proxy — so the
// alternative would be a catalog resolution on every request to disambiguate a
// name no Copilot model has.
//
// An empty suffix ("gpt-5:") is neither: it cannot be an effort, and a trailing
// colon is not part of any model name, so it is reported as a truncated selector
// instead of being looked up.
func ParseModelSelector(raw string) (ModelSelector, error) {
	if raw == "" {
		return ModelSelector{}, apierr.InvalidRequest("model is required", "model")
	}
	separator := strings.LastIndex(raw, ":")
	if separator < 0 {
		return ModelSelector{Model: raw}, nil
	}

	effort := NormalizeReasoningEffort(raw[separator+1:])
	if effort == "" {
		return ModelSelector{}, apierr.InvalidRequest("model reasoning effort suffix must not be empty", "model")
	}
	if !IsKnownReasoningEffort(effort) {
		return ModelSelector{Model: raw}, nil
	}
	model := raw[:separator]
	if model == "" {
		return ModelSelector{}, apierr.InvalidRequest("model selector must include a model before the reasoning effort suffix", "model")
	}
	return ModelSelector{Model: model, ReasoningEffort: effort, HasEffort: true}, nil
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
		return "", apierr.InvalidRequest("model reasoning effort suffix conflicts with "+explicitParam, explicitParam)
	}
	return selector.ReasoningEffort, nil
}
