package sessionfs

import "testing"

// soakIterations scales a soak loop's iteration count for -short runs.
//
// The durability tests in this package are fsync-bound, not CPU-bound: every
// WriteFile is a write, an fsync, a rename and a parent-directory fsync, and Go
// issues F_FULLFSYNC on macOS, which asks the drive to flush its own write
// cache. Measured on an APFS volume that is 4.87 ms per sync against 0.08 ms
// for the same write without one - a 60x penalty that no amount of test
// tidying removes, because the fsync is the thing under test.
//
// So the loops stay long by default: they are what caught the mid-write symlink
// swap (reproduced 5/5 before the os.Root fix) and the torn-read window, and
// both need enough attempts to hit a narrow race. -short trades that soak depth
// for a fast inner loop while still executing every assertion at least a few
// times, so a developer iterating locally is not paying seconds per run.
//
// Use `go test -short ./...` while iterating; CI runs the full count.
func soakIterations(full int) int {
	if testing.Short() {
		return max(full/25, 2)
	}
	return full
}
