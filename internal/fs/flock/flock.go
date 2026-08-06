// Package flock takes advisory file locks with flock(2).

package flock

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tomdoesdev/dac/internal/fs/flock/internal/sys"
)

// ErrLocked reports that another holder has the lock.
// TryAcquire returns it. Acquire waits instead of returning it.
var ErrLocked = errors.New("another process holds the lock")

// Mode is the kind of lock to take.
type Mode int

const (
	// Exclusive admits one holder.
	Exclusive Mode = iota
	// Shared admits other shared holders and excludes an exclusive holder.
	Shared
)

func (mode Mode) String() string {
	if mode == Shared {
		return "shared"
	}
	return "exclusive"
}

// Lock is one held file lock.
// A Lock always holds its lock until Release. There is no unlocked state.
type Lock struct {
	file    *os.File
	path    string
	mode    Mode
	ownFile bool

	once sync.Once
	err  error
}

// Acquire waits for the lock, the end of the context, or a failure.
// It creates the file and its parent directories unless WithoutCreate says not to.
func Acquire(ctx context.Context, path string, options ...Option) (*Lock, error) {
	settings := newSettings(options)
	file, err := open(path, settings)
	if err != nil {
		return nil, err
	}
	lock, err := acquire(ctx, file, path, true, settings)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return lock, nil
}

// TryAcquire makes one attempt and never waits.
// It returns an error that matches ErrLocked when another holder has the lock.
func TryAcquire(path string, options ...Option) (*Lock, error) {
	settings := newSettings(options)
	file, err := open(path, settings)
	if err != nil {
		return nil, err
	}
	if err := sys.Lock(file.Fd(), settings.mode == Shared, false); err != nil {
		_ = file.Close()
		return nil, pathError(path, translate(err))
	}
	return newLock(file, path, true, settings.mode), nil
}

// AcquireFile locks a file that the caller opened.
// Release unlocks that file, and Release does not close it.
// This is the way to lock a directory, because Acquire opens for writing.
func AcquireFile(ctx context.Context, file *os.File, options ...Option) (*Lock, error) {
	return acquire(ctx, file, file.Name(), false, newSettings(options))
}

// Hold runs work while it holds the lock.
// It releases the lock after work returns, and it joins the two errors.
func Hold(ctx context.Context, path string, work func(context.Context) error, options ...Option) error {
	lock, err := Acquire(ctx, path, options...)
	if err != nil {
		return err
	}
	workErr := work(ctx)
	return errors.Join(workErr, lock.Release())
}

// Release releases the lock. Release is idempotent.
// A second call returns the result of the first call and does nothing more.
func (lock *Lock) Release() error {
	lock.once.Do(func() { lock.err = lock.release() })
	return lock.err
}

// Path reports the locked path.
func (lock *Lock) Path() string { return lock.path }

// Mode reports Exclusive or Shared.
func (lock *Lock) Mode() Mode { return lock.mode }

// File reports the open lock file.
// The caller must not close it, because Release closes it.
func (lock *Lock) File() *os.File { return lock.file }

func (lock *Lock) String() string {
	return lock.mode.String() + " lock on " + lock.path
}

// release unlocks the file, and closes the file when this package opened it.
// A close releases the lock even when the unlock reports a failure, so the
// close error only matters when the unlock succeeded.
func (lock *Lock) release() error {
	unlockErr := sys.Unlock(lock.file.Fd())
	if !lock.ownFile {
		if unlockErr != nil {
			return pathError(lock.path, unlockErr)
		}
		return nil
	}
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return pathError(lock.path, unlockErr)
	}
	return closeErr
}

// acquire takes the lock, and waits with a backoff while another holder has it.
//
// The wait polls. The alternative is a blocking flock(2) call in a goroutine.
// That goroutine then stays parked until the lock becomes free, which a
// cancelled context cannot stop and which a long-lived process cannot afford.
func acquire(ctx context.Context, file *os.File, path string, ownFile bool, settings settings) (*Lock, error) {
	delay := settings.firstDelay
	for {
		err := sys.Lock(file.Fd(), settings.mode == Shared, false)
		if err == nil {
			return newLock(file, path, ownFile, settings.mode), nil
		}
		if !errors.Is(err, sys.ErrWouldBlock) {
			return nil, pathError(path, err)
		}
		select {
		case <-ctx.Done():
			return nil, pathError(path, ctx.Err())
		case <-time.After(delay):
		}
		delay = min(delay*2, settings.limitDelay)
	}
}

func newLock(file *os.File, path string, ownFile bool, mode Mode) *Lock {
	return &Lock{file: file, path: path, mode: mode, ownFile: ownFile}
}

// open opens the lock file for reading and writing.
// The file is open for writing so that a caller can write to File.
func open(path string, settings settings) (*os.File, error) {
	flags := os.O_RDWR
	if settings.create {
		if err := os.MkdirAll(filepath.Dir(path), settings.dirMode); err != nil {
			return nil, err
		}
		flags |= os.O_CREATE
	}
	// os.OpenFile already reports the path.
	return os.OpenFile(path, flags, settings.fileMode)
}

// translate turns the low-level sentinel into the error that callers test for.
func translate(err error) error {
	if errors.Is(err, sys.ErrWouldBlock) {
		return ErrLocked
	}
	return err
}

// pathError names the file in every error this package makes.
// fs.PathError unwraps, so errors.Is still finds ErrLocked, fs.ErrPermission,
// and context.DeadlineExceeded.
func pathError(path string, err error) error {
	return &fs.PathError{Op: "flock", Path: path, Err: err}
}
