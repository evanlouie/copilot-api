package openai

import (
	"strings"
	"testing"
)

// The grammar has to accept everything NewID mints, or a legitimately created
// response becomes unreadable through GET /v1/responses/{id}.
func TestValidResponseIDAcceptsEveryMintedID(t *testing.T) {
	t.Parallel()
	for range 100 {
		id := NewID(ResponseIDPrefix)
		if !ValidResponseID(id) {
			t.Fatalf("minted id %q rejected by its own grammar", id)
		}
	}
}

func TestValidResponseIDRejectsAnythingThatCouldNameAnotherPath(t *testing.T) {
	t.Parallel()
	for _, id := range []string{
		"",
		".",
		"..",
		"resp_",
		"resp_.",
		"resp_..",
		"resp_../../etc/passwd",
		"resp_a/b",
		"resp_a.b",
		"resp_a b",
		"resp_" + strings.Repeat("a", 129),
		"chatcmpl_1",
		"1",
	} {
		if ValidResponseID(id) {
			t.Fatalf("ValidResponseID(%q) = true, want false", id)
		}
	}
}
