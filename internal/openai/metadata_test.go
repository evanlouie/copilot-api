package openai

import (
	"strings"
	"testing"
)

// TestMetadataLimitsMatchTheRealAPI pins the bounds rather than truncating.
//
// metadata is round-trippable state the client reads back, so a truncated
// value looks valid and correlates to nothing - worse than a 400 the client
// can act on.
func TestMetadataLimitsMatchTheRealAPI(t *testing.T) {
	t.Parallel()
	pairs := func(n int) map[string]string {
		out := make(map[string]string, n)
		for i := range n {
			out[string(rune('a'+i%26))+strings.Repeat("x", i/26+1)] = "v"
		}
		return out
	}
	for _, tc := range []struct {
		name     string
		metadata map[string]string
		wantErr  bool
	}{
		{"absent", nil, false},
		{"empty", map[string]string{}, false},
		{"ordinary", map[string]string{"trace_id": "abc-123"}, false},
		{"at the pair limit", pairs(16), false},
		{"over the pair limit", pairs(17), true},
		{"key at the limit", map[string]string{strings.Repeat("k", 64): "v"}, false},
		{"key over the limit", map[string]string{strings.Repeat("k", 65): "v"}, true},
		{"value at the limit", map[string]string{"k": strings.Repeat("v", 512)}, false},
		{"value over the limit", map[string]string{"k": strings.Repeat("v", 513)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateMetadata(tc.metadata)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateMetadata(%d pairs) = nil, want a rejection", len(tc.metadata))
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateMetadata = %v, want accepted", err)
			}
			if err != nil && !strings.Contains(err.Error(), "metadata") {
				t.Fatalf("error = %v, want it to name the metadata field", err)
			}
		})
	}
}
