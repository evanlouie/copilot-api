package openai

import (
	"encoding/json"
	"testing"
)

func TestParseModelSelector(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		model     string
		effort    string
		hasEffort bool
		wantErr   bool
	}{
		{name: "naked model", raw: "gpt-5.6-sol", model: "gpt-5.6-sol"},
		{name: "none", raw: "gpt-5.6-sol:none", model: "gpt-5.6-sol", effort: "none", hasEffort: true},
		{name: "minimal", raw: "gpt-5.6-sol:minimal", model: "gpt-5.6-sol", effort: "minimal", hasEffort: true},
		{name: "low", raw: "gpt-5.6-sol:low", model: "gpt-5.6-sol", effort: "low", hasEffort: true},
		{name: "medium", raw: "gpt-5.6-sol:medium", model: "gpt-5.6-sol", effort: "medium", hasEffort: true},
		{name: "high", raw: "gpt-5.6-sol:high", model: "gpt-5.6-sol", effort: "high", hasEffort: true},
		{name: "xhigh", raw: "gpt-5.6-sol:xhigh", model: "gpt-5.6-sol", effort: "xhigh", hasEffort: true},
		{name: "normalizes effort", raw: "gpt-5.6-sol:  XHIGH  ", model: "gpt-5.6-sol", effort: "xhigh", hasEffort: true},
		{name: "uses final colon", raw: "vendor:model:HIGH", model: "vendor:model", effort: "high", hasEffort: true},
		{name: "future model specific effort", raw: "gpt-5.6-sol:adaptive", model: "gpt-5.6-sol", effort: "adaptive", hasEffort: true},
		{name: "empty model", raw: "", wantErr: true},
		{name: "suffix only", raw: ":high", wantErr: true},
		{name: "empty suffix", raw: "gpt-5.6-sol:", wantErr: true},
		{name: "whitespace suffix", raw: "gpt-5.6-sol:   ", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseModelSelector(test.raw)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ParseModelSelector(%q) succeeded: %#v", test.raw, got)
				}
				apiErr, ok := err.(*APIError)
				if !ok || apiErr.Type != "invalid_request_error" || apiErr.Param != "model" {
					t.Fatalf("ParseModelSelector(%q) error = %#v, want invalid_request_error on model", test.raw, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseModelSelector(%q): %v", test.raw, err)
			}
			if got.Model != test.model || got.ReasoningEffort != test.effort || got.HasEffort != test.hasEffort {
				t.Fatalf("ParseModelSelector(%q) = %#v, want model=%q effort=%q hasEffort=%t", test.raw, got, test.model, test.effort, test.hasEffort)
			}
		})
	}
}

func TestMergeReasoningEffort(t *testing.T) {
	naked := ModelSelector{Model: "gpt-5"}
	suffixed := ModelSelector{Model: "gpt-5", ReasoningEffort: "xhigh", HasEffort: true}
	tests := []struct {
		name     string
		selector ModelSelector
		explicit string
		want     string
		wantErr  bool
	}{
		{name: "omitted", selector: naked},
		{name: "normalizes explicit", selector: naked, explicit: " HIGH ", want: "high"},
		{name: "suffix only", selector: suffixed, want: "xhigh"},
		{name: "matching", selector: suffixed, explicit: " XHIGH ", want: "xhigh"},
		{name: "conflicting", selector: suffixed, explicit: "low", wantErr: true},
		{name: "blank explicit remains omitted", selector: suffixed, explicit: "  ", want: "xhigh"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := MergeReasoningEffort(test.selector, test.explicit, "reasoning_effort")
			if test.wantErr {
				if err == nil {
					t.Fatalf("MergeReasoningEffort() = %q, want error", got)
				}
				apiErr, ok := err.(*APIError)
				if !ok || apiErr.Param != "reasoning_effort" || apiErr.Type != "invalid_request_error" {
					t.Fatalf("MergeReasoningEffort() error = %#v, want invalid_request_error on reasoning_effort", err)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("MergeReasoningEffort() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestResponsesReasoningEffortUsesNormalizedConflictComparison(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    string
		wantErr bool
	}{
		{
			name: "matching top level and nested",
			body: `{"model":"gpt-5","reasoning_effort":" XHIGH ","reasoning":{"effort":"xhigh"},"input":"hi"}`,
			want: "xhigh",
		},
		{
			name: "nested value normalized",
			body: `{"model":"gpt-5","reasoning":{"effort":" HIGH "},"input":"hi"}`,
			want: "high",
		},
		{
			name:    "conflicting values",
			body:    `{"model":"gpt-5","reasoning_effort":"high","reasoning":{"effort":"low"},"input":"hi"}`,
			wantErr: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var req ResponsesRequest
			if err := json.Unmarshal([]byte(test.body), &req); err != nil {
				t.Fatal(err)
			}
			err := ValidateResponsesRequest(&req, false)
			if test.wantErr {
				apiErr, ok := err.(*APIError)
				if !ok || apiErr.Param != "reasoning.effort" {
					t.Fatalf("ValidateResponsesRequest() error = %#v, want reasoning.effort conflict", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := ResponsesReasoningEffort(&req); got != test.want {
				t.Fatalf("ResponsesReasoningEffort() = %q, want %q", got, test.want)
			}
		})
	}
}
