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
// The live SDK owns two process-wait goroutines that can outlive a completed
// test while its CLI child is being reaped: os/exec's pipe copier, spawned from
// Cmd.Start, and the Copilot SDK's monitorProcess waiter. They are ignored only
// when the live gate is enabled, and by their exact owning stack functions, so
// the normal offline suite retains unqualified leak detection and every gateway-
// owned loop remains visible in live runs too.
//
// Any entry added below must name the loop it covers and say why that loop
// legitimately outlives a test. An unexplained ignore is how leak detection
// stops detecting leaks.
func TestMain(m *testing.M) {
	var options []goleak.Option
	if os.Getenv(liveTestsEnv) == "1" {
		options = append(options,
			goleak.IgnoreAnyFunction("os/exec.(*Cmd).Start.func2"),
			goleak.IgnoreAnyFunction("github.com/github/copilot-sdk/go.(*Client).monitorProcess.func1"),
		)
	}
	goleak.VerifyTestMain(m, options...)
}
