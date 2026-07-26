package sessionstore

import "testing"

// soakIterations scales a soak loop's iteration count for -short runs.
//
// The store's tests are fsync-bound, not CPU-bound: every SaveResponse writes a
// temp file, fsyncs it, renames it and fsyncs the parent directory, and Go
// issues F_FULLFSYNC on macOS, which asks the drive to flush its own write
// cache. Measured on an APFS volume that is 4.87 ms per sync against 0.08 ms
// for the same write without one - a 60x penalty that is inherent, because
// durability is the property under test.
//
// So the loops stay long by default: TestPinnedResponseSurvivesConcurrentPrune
// needs enough saves to actually collide with a concurrent pruner, which is how
// the pin-versus-retention race was pinned down in the first place. -short
// trades that soak depth for a fast inner loop while still executing every
// assertion, so a developer iterating locally is not paying seconds per run.
//
// Use `go test -short ./...` while iterating; CI runs the full count.
func soakIterations(full int) int {
	if testing.Short() {
		return max(full/25, 2)
	}
	return full
}
