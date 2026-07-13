package observability

import (
	"strings"
	"testing"
)

func TestTruncateForLogPreservesRuneBoundaries(t *testing.T) {
	if got := TruncateForLog("a😀bc", 2); got != "a😀…" {
		t.Fatalf("TruncateForLog = %q, want %q", got, "a😀…")
	}
	if got := TruncateForLog("short", 10); got != "short" {
		t.Fatalf("TruncateForLog changed short input: %q", got)
	}
}

func TestTruncateBytesForLogPreservesRuneBoundaries(t *testing.T) {
	if got := TruncateBytesForLog([]byte("a😀bc"), 2); got != "a😀…" {
		t.Fatalf("TruncateBytesForLog = %q, want %q", got, "a😀…")
	}
}

func BenchmarkTruncateBytesForLogLargePayload(b *testing.B) {
	payload := []byte(strings.Repeat("x", 100<<20))
	b.ReportAllocs()
	for range b.N {
		_ = TruncateBytesForLog(payload, 240)
	}
}
