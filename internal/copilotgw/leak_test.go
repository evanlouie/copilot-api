package copilotgw

import (
	"os"
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
	var opts []goleak.Option
	if os.Getenv("COPILOT_API_LIVE_TESTS") == "1" {
		// The Copilot SDK owns both goroutines and can leave them alive briefly
		// after Client.Stop returns: os/exec's command-context watcher and the
		// SDK's process waiter. They are present only in live child-process
		// sessions, so normal unit-test leak detection remains unfiltered.
		opts = append(opts,
			goleak.IgnoreAnyFunction("os/exec.(*Cmd).watchCtx"),
			goleak.IgnoreAnyFunction("github.com/github/copilot-sdk/go.(*Client).monitorProcess.func1"),
		)
	}
	goleak.VerifyTestMain(m, opts...)
}
