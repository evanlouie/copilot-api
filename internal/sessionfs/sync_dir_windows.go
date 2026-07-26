//go:build windows

package sessionfs

import "os"

// Windows rename durability is provided by the file flush; opening directories
// for fsync is not supported by os.Open.
func syncDirectory(*os.Root, string) error { return nil }
