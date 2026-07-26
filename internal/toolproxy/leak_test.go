package toolproxy

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain fails this package if a test leaves a goroutine behind.
//
// A tool-call batch parks the SDK's tool handler goroutine until the client
// returns outputs, the batch TTL expires or the turn is cancelled. Those
// handlers are the thing this package exists to manage, so one that never
// unparks is precisely the bug worth failing a test over.
//
// Any entry added below must name the loop it covers and say why that loop
// legitimately outlives a test. An unexplained ignore is how leak detection
// stops detecting leaks.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
