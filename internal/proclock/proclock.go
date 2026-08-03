//go:build unix

// Package proclock coordinates DAC processes with file locks.
//
// DAC targets Unix. The build constraint makes that explicit, so a Windows
// build fails with a missing-package error instead of a missing-symbol error
// from deep inside the cache.
package proclock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Lock holds an exclusive process lock.
type Lock struct {
	file *os.File
}

// Acquire waits for an exclusive lock or context cancellation.
func Acquire(ctx context.Context, path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Lock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// Release releases the process lock.
//
// It always releases: closing the file drops the lock even when the explicit
// unlock fails, so the returned error only ever describes a file descriptor
// the caller has finished with, and never a lock that is still held.
func (lock *Lock) Release() error {
	unlockErr := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
