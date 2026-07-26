package openai

import (
	"encoding/json"
	"sort"
)

// Fields this proxy accepts and does not act on.
//
// The permissive validation policy is "reject unknown; accept-and-ignore
// known-but-unsupported, but only when ignoring degrades gracefully". The
// accept-and-ignore half is only defensible if it is observable, and it was
// not: deleting strict mode removed the one way an operator could discover
// that a client was sending sampling knobs this backend has no control for.
// Naming them here is what turns "silently ignored" into "ignored, and the log
// says so".
//
// A field belongs here only while nothing reads it. Anything this proxy starts
// honouring - metadata did - must come off the list, or the log starts lying in
// the other direction.
var (
	unhonoredChatFields = []string{
		"audio", "frequency_penalty", "function_call", "functions", "logit_bias",
		"logprobs", "modalities", "n", "prediction", "presence_penalty", "seed",
		"service_tier", "stop", "temperature", "top_logprobs", "top_p", "user",
	}
	unhonoredResponseFields = []string{
		"background", "service_tier", "temperature", "top_p", "truncation", "user",
	}
)

// UnhonoredChatFields returns the accepted-but-ignored fields present on a Chat
// request, sorted so the log line is stable.
func UnhonoredChatFields(raw map[string]json.RawMessage) []string {
	return presentUnhonored(raw, unhonoredChatFields)
}

// UnhonoredResponseFields returns the accepted-but-ignored fields present on a
// Responses request, sorted so the log line is stable.
func UnhonoredResponseFields(raw map[string]json.RawMessage) []string {
	return presentUnhonored(raw, unhonoredResponseFields)
}

func presentUnhonored(raw map[string]json.RawMessage, known []string) []string {
	if len(raw) == 0 {
		return nil
	}
	var out []string
	for _, name := range known {
		if _, ok := raw[name]; ok {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
