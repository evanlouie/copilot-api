//go:build windows

package sessionstore

// Windows rename durability is provided by the file flush; opening directories
// for fsync is not supported by os.Open.
func syncDirectory(string) error { return nil }
