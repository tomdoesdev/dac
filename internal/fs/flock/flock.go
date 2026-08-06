// Package flock takes advisory file locks with flock(2).
//
// Acquire returns a Lock that holds its lock. There is no unlocked state to
// mishandle, and no predicate that reports a lock state which the next
// instruction can change.
//
//	err := flock.Hold(ctx, "/var/lib/app/state.lock", func(ctx context.Context) error {
//		return rewrite(ctx)
//	})
//
// Use TryAcquire to refuse a wait. It returns ErrLocked at once:
//
//	lock, err := flock.TryAcquire(path)
//	if errors.Is(err, flock.ErrLocked) {
//		fmt.Println("another process is busy with this file")
//	}
//
// # What flock(2) does
//
// Five properties of flock(2) surprise callers. Each one is a property of the
// system call, not a choice this package made.
//
// The lock belongs to the open file description, not to the process. Two
// Acquire calls in one process open the file two times, so the two locks
// exclude each other. One goroutine that calls Acquire two times on one path
// waits for itself until its context ends.
//
// Never delete a lock file. A process that deletes the file leaves the holders
// locked on the old inode. The next process makes a new inode and locks a
// different object, and mutual exclusion stops. This package supplies no
// remove function for that reason. A lock file is empty, so the cost of an
// abandoned one is one inode.
//
// Lock a sidecar file, and not a file that a rename replaces. An atomic write
// replaces the target inode, so a lock on the old inode protects nothing.
// Lock "state.json.lock" while you write "state.json".
//
// The lock is advisory. A process that does not call flock reads and writes
// the file freely.
//
// A lock survives fork. A lock does not survive exec when the descriptor has
// O_CLOEXEC, which is the default for a file that Go opens.
//
// # Platforms
//
// This package needs flock(2). It works on darwin, dragonfly, freebsd,
// illumos, linux, netbsd, and openbsd. Every function builds on every
// platform and returns errors.ErrUnsupported on the others, so a program that
// imports this package still builds for Windows.
package flock

import (
	"context"
	"io/fs"
	"os"
	"time"

	"github.com/tomdoesdev/dac/internal/fs/flock/internal/lockfile"
)

// Lock is one held file lock.
//
// Release releases the lock and is idempotent. Path reports the locked path.
// Mode reports Exclusive or Shared. File reports the open lock file, which
// the caller must not close.
type Lock = lockfile.Lock

// Mode is the kind of lock to take.
type Mode = lockfile.Mode

const (
	// Exclusive admits one holder.
	Exclusive = lockfile.Exclusive
	// Shared admits other shared holders and excludes an exclusive holder.
	Shared = lockfile.Shared
)

// ErrLocked reports that another holder has the lock.
// TryAcquire returns it. Acquire waits instead of returning it.
var ErrLocked = lockfile.ErrLocked

// Option changes how this package takes a lock.
type Option = lockfile.Option

// Acquire waits for the lock, the end of the context, or a failure.
// It creates the file and its parent directories unless WithoutCreate says not to.
func Acquire(ctx context.Context, path string, options ...Option) (*Lock, error) {
	return lockfile.Acquire(ctx, path, options...)
}

// TryAcquire makes one attempt and never waits.
// It returns an error that matches ErrLocked when another holder has the lock.
func TryAcquire(path string, options ...Option) (*Lock, error) {
	return lockfile.TryAcquire(path, options...)
}

// AcquireFile locks a file that the caller opened.
// Release unlocks that file, and Release does not close it.
// This is the way to lock a directory, because Acquire opens for writing.
func AcquireFile(ctx context.Context, file *os.File, options ...Option) (*Lock, error) {
	return lockfile.AcquireFile(ctx, file, options...)
}

// Hold runs work while it holds the lock.
// It releases the lock after work returns, and it joins the two errors.
func Hold(ctx context.Context, path string, work func(context.Context) error, options ...Option) error {
	return lockfile.Hold(ctx, path, work, options...)
}

// WithMode selects Exclusive or Shared. The default is Exclusive.
func WithMode(mode Mode) Option { return lockfile.WithMode(mode) }

// WithFileMode sets the mode of a lock file that this package creates.
// The default is 0o600. The mode of a file that exists already does not change.
func WithFileMode(mode fs.FileMode) Option { return lockfile.WithFileMode(mode) }

// WithDirMode sets the mode of a directory that this package creates.
// The default is 0o755.
func WithDirMode(mode fs.FileMode) Option { return lockfile.WithDirMode(mode) }

// WithoutCreate stops this package from making the file and its directory.
// A path that does not exist then fails with fs.ErrNotExist.
func WithoutCreate() Option { return lockfile.WithoutCreate() }

// WithBackoff sets the first wait and the longest wait between two attempts.
// The default is 1 ms to 32 ms.
func WithBackoff(first, limit time.Duration) Option { return lockfile.WithBackoff(first, limit) }
