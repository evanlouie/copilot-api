//go:build windows

package sessionstore

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

type Lock struct {
	file       *os.File
	path       string
	overlapped syscall.Overlapped
}

func AcquireLock(path string) (*Lock, error) {
	if err := os.MkdirAll(dirname(path), 0o700); err != nil {
		return nil, err
	}
	// The file is only a rendezvous point. Ownership is held by the Windows
	// kernel lock, which is released automatically if the process crashes; a
	// stale file therefore cannot permanently block restart.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	lock := &Lock{file: f, path: path}
	r1, _, callErr := procLockFileEx.Call(
		f.Fd(),
		lockfileExclusiveLock|lockfileFailImmediately,
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&lock.overlapped)),
	)
	if r1 == 0 {
		_ = f.Close()
		return nil, fmt.Errorf("store lock is already held at %s: %w", path, callErr)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.Seek(0, 0)
		_, _ = fmt.Fprintf(f, "pid=%d\n", os.Getpid())
	}
	return lock, nil
}

func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_, _, unlockErr := procUnlockFileEx.Call(
		l.file.Fd(),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&l.overlapped)),
	)
	closeErr := l.file.Close()
	_ = os.Remove(l.path)
	l.file = nil
	if unlockErr != syscall.Errno(0) {
		return unlockErr
	}
	return closeErr
}

func dirname(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			if i == 0 {
				return path[:1]
			}
			return path[:i]
		}
	}
	return "."
}
