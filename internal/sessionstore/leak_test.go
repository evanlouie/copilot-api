package sessionstore

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails this package if a test leaves a goroutine behind.
//
// The store's retention pins and lifecycle locks are released by the
// goroutines that took them, so a goroutine that never finishes is state that
// is never unpinned and a lock file that is never dropped.
//
// Any entry added below must name the loop it covers and say why that loop
// legitimately outlives a test. An unexplained ignore is how leak detection
// stops detecting leaks.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
