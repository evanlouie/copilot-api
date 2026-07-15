//go:build windows

package sessionstore

import (
	"os"
	"os/exec"
	"testing"
)

func TestWindowsLockRecoversAfterOwnerExit(t *testing.T) {
	if path := os.Getenv("COPILOT_API_LOCK_CRASH_HELPER"); path != "" {
		if _, err := AcquireLock(path); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	path := New(t.TempDir(), t.TempDir(), t.TempDir()).LockPath()
	command := exec.Command(os.Args[0], "-test.run=TestWindowsLockRecoversAfterOwnerExit")
	command.Env = append(os.Environ(), "COPILOT_API_LOCK_CRASH_HELPER="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("helper failed: %v: %s", err, output)
	}
	lock, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("stale lock blocked recovery: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}
