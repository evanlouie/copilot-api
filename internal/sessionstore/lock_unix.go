//go:build !windows

package sessionstore

import (
	"fmt"
	"os"
	"syscall"
)

type Lock struct {
	file *os.File
	path string
}

func AcquireLock(path string) (*Lock, error) {
	if err := os.MkdirAll(dirname(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("store lock is already held at %s", path)
	}
	_ = f.Truncate(0)
	_, _ = fmt.Fprintf(f, "pid=%d\n", os.Getpid())
	return &Lock{file: f, path: path}, nil
}

func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}

func dirname(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			if i == 0 {
				return "/"
			}
			return path[:i]
		}
	}
	return "."
}
