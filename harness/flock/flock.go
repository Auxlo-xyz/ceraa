package flock

import (
	"os"
	"syscall"
)

// Flock maps to soraacode util/flock.ts + effect-flock.ts
// Simple file lock via syscall.Flock

type Lock struct {
	file *os.File
	path string
}

// Acquire acquires exclusive lock, blocks until available
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Lock{file: f, path: path}, nil
}

// TryAcquire tries non-blocking
func TryAcquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Lock{file: f, path: path}, nil
}

// Release unlocks and closes
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return l.file.Close()
}
