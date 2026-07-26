package copilotgw

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails this package if a test leaves a goroutine behind.
//
// Every turn this gateway runs is a goroutine that owns an SDK session, a
// tool-call batch and a set of sessionstore retention pins, and the only thing
// that releases them is that goroutine reaching its cleanup. A leak here is a
// session that is never disconnected and state that is never pruned, so it
// should fail the test rather than accumulate in production.
//
// Any entry added below must name the loop it covers and say why that loop
// legitimately outlives a test. An unexplained ignore is how leak detection
// stops detecting leaks.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
