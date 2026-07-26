package openai

import (
	"strconv"
	"strings"

	"github.com/evanlouie/copilot-api/internal/apierr"
)

// Reasoning effort reaches this proxy in three spellings — the `model` suffix,
// Chat's `reasoning_effort`, and the Responses `reasoning.effort` object — and
// all three are backed by the one set below. A second hand-written list would
// drift, and drift here is silent: a value one spelling accepts and another
// drops changes how much the model thinks with nothing on the wire to say so.

// reasoningEfforts is the canonical enum, ordered from least to most
// deliberation. The order is meaningful: copilotgw snaps a configured default
// onto the efforts a model advertises by walking these ranks.
//
// It is the union of what both sides can say, not just OpenAI's list. "minimal"
// is OpenAI's; "xhigh" and "max" are Copilot's - the SDK names the latter in
// its own documentation for AssistantUsageData.ReasoningEffort ("none", "low",
// "medium", "high", "xhigh", "max"). Missing one is not harmless: the model
// catalog's SupportedReasoningEfforts is an unconstrained string list, and this
// set gates both the `model:effort` aliases GET /v1/models publishes and the
// values POST accepts, so a token in one and not the other means advertising an
// id that is then rejected.
var reasoningEfforts = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

var reasoningEffortRanks = func() map[string]int {
	ranks := make(map[string]int, len(reasoningEfforts))
	for rank, effort := range reasoningEfforts {
		ranks[effort] = rank
	}
	return ranks
}()

// NormalizeReasoningEffort applies the normalization used for every reasoning
// effort spelling at the OpenAI compatibility boundary.
func NormalizeReasoningEffort(effort string) string {
	return strings.ToLower(strings.TrimSpace(effort))
}

// KnownReasoningEfforts returns the canonical enum in ascending order.
func KnownReasoningEfforts() []string {
	return append([]string(nil), reasoningEfforts...)
}

// IsKnownReasoningEffort reports whether effort names a canonical effort.
func IsKnownReasoningEffort(effort string) bool {
	_, ok := reasoningEffortRanks[NormalizeReasoningEffort(effort)]
	return ok
}

// ReasoningEffortRank returns the position of effort in the canonical order,
// and whether it is a member at all.
func ReasoningEffortRank(effort string) (int, bool) {
	rank, ok := reasoningEffortRanks[NormalizeReasoningEffort(effort)]
	return rank, ok
}

// ValidateReasoningEffort rejects a value outside the canonical enum on the
// param the caller's surface spells it with. An omitted effort is not a value
// and is accepted.
//
// This is the reject half of the validation policy in validation.go rather than
// the accept-and-ignore half: reasoning effort is a control this proxy acts on,
// so ignoring an unusable value would run the turn at some other effort than the
// client asked for and bill them for it, which is not a graceful degradation.
func ValidateReasoningEffort(effort, param string) error {
	normalized := NormalizeReasoningEffort(effort)
	if normalized == "" {
		return nil
	}
	if _, ok := reasoningEffortRanks[normalized]; ok {
		return nil
	}
	return apierr.InvalidRequest("unknown "+param+" "+strconv.Quote(normalized)+"; supported values are "+strings.Join(reasoningEfforts, ", "), param)
}
