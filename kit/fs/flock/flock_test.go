package flock_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomdoesdev/kit/fs/flock"
)

// TestLockExcludesAndReleases defines the basic exclusive-lock lifecycle. A
// contender must remain blocked while the first handle owns the lock, honor its
// context deadline, and acquire successfully after Release.
func TestLockExcludesAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object.lock")
	first, err := flock.Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	// A second acquisition must wait, so a bounded context must expire.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := flock.Acquire(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a held lock was acquired twice: %v", err)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := flock.Acquire(context.Background(), path)
	if err != nil {
		t.Fatalf("a released lock could not be acquired: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestHiddenPathKeepsTheLockBesideItsFile pins sidecar naming. Locks must share
// a directory with the protected file so rename and permission boundaries stay
// local, and an already-hidden filename must not gain a confusing second dot.
func TestHiddenPathKeepsTheLockBesideItsFile(t *testing.T) {
	directory := t.TempDir()
	if got := flock.HiddenPath(filepath.Join(directory, "dac.json")); got != filepath.Join(directory, ".dac.json.lock") {
		t.Fatalf("HiddenPath returned %q", got)
	}
	if got := flock.HiddenPath(filepath.Join(directory, ".config")); got != filepath.Join(directory, ".config.lock") {
		t.Fatalf("HiddenPath added a second dot: %q", got)
	}
}

// TestLockSerializesHolders stress-tests mutual exclusion rather than checking
// only two sequential acquisitions. The peak counter catches any overlap among
// eight goroutines that would expose DAC's atomic transactions to races.
func TestLockSerializesHolders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object.lock")
	var holders, peak atomic.Int32
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			lock, err := flock.Acquire(context.Background(), path)
			if err != nil {
				t.Errorf("Acquire returned %v", err)
				return
			}
			current := holders.Add(1)
			for {
				highest := peak.Load()
				if current <= highest || peak.CompareAndSwap(highest, current) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			holders.Add(-1)
			if err := lock.Release(); err != nil {
				t.Errorf("Release returned %v", err)
			}
		})
	}
	group.Wait()
	if got := peak.Load(); got != 1 {
		t.Fatalf("%d holders held the lock at once", got)
	}
}

// TestRemoveOnReleaseSerializesHoldersAndRemovesTheFile exercises transient lock
// cleanup under heavy contention. Removing sidecars must not open a gap that
// admits concurrent holders, and the final releaser must leave no lock file.
func TestRemoveOnReleaseSerializesHoldersAndRemovesTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".object.lock")
	var holders, peak atomic.Int32
	var group sync.WaitGroup
	for range 32 {
		group.Go(func() {
			err := flock.Hold(context.Background(), path, func(context.Context) error {
				current := holders.Add(1)
				defer holders.Add(-1)
				for {
					highest := peak.Load()
					if current <= highest || peak.CompareAndSwap(highest, current) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				return nil
			}, flock.RemoveOnRelease())
			if err != nil {
				t.Errorf("Hold returned %v", err)
			}
		})
	}
	group.Wait()
	if got := peak.Load(); got != 1 {
		t.Fatalf("%d holders held a transient lock at once", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("transient lock file survived release: %v", err)
	}
}

// TestRemoveOnReleaseDoesNotDeleteAReplacementPath prevents pathname-based
// cleanup from deleting someone else's file. If the locked inode is unlinked
// and the path is reused, Release may remove only the inode it originally owned.
func TestRemoveOnReleaseDoesNotDeleteAReplacementPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".object.lock")
	lock, err := flock.Acquire(context.Background(), path, flock.RemoveOnRelease())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "replacement" {
		t.Fatalf("replacement path holds %q: %v", data, err)
	}
}

// TestRemoveOnReleaseRejectsSharedLocks rules out an unsafe option combination.
// One shared reader cannot know it is the final holder, so allowing it to unlink
// the lock path could let a new inode and a second lock domain appear. Rejection
// must occur before creating the sidecar.
func TestRemoveOnReleaseRejectsSharedLocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".object.lock")
	_, err := flock.Acquire(context.Background(), path,
		flock.WithMode(flock.Shared), flock.RemoveOnRelease())
	if !errors.Is(err, flock.ErrRemoveOnReleaseShared) {
		t.Fatalf("Acquire error = %v, want ErrRemoveOnReleaseShared", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("rejected settings created a lock file: %v", statErr)
	}
}

// TestAcquireCreatesMissingDirectories documents the default convenience used by
// DAC sidecars: acquiring a nested lock bootstraps its parent path and lock file.
func TestAcquireCreatesMissingDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "nested", "object.lock")
	lock, err := flock.Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock file was not created: %v", err)
	}
}

// TestAcquireRespectsCancelledContext ensures lock contention does not make DAC
// unresponsive to cancellation. A context canceled before acquisition should
// return context.Canceled rather than wait for the current owner.
func TestAcquireRespectsCancelledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object.lock")
	held, err := flock.Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := flock.Acquire(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled acquisition returned %v", err)
	}
}

// TryAcquire reports contention with no wait, so a test of contention needs no
// deadline and cannot go slow on a loaded machine.
func TestTryAcquireReportsAHeldLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object.lock")
	held, err := flock.TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := flock.TryAcquire(path); !errors.Is(err, flock.ErrLocked) {
		t.Fatalf("a held lock returned %v, want ErrLocked", err)
	}

	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	next, err := flock.TryAcquire(path)
	if err != nil {
		t.Fatalf("a released lock returned %v", err)
	}
	if err := next.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestSharedLocksAdmitOtherReaders verifies the positive half of reader/writer
// semantics: multiple readers may coexist, and the acquired handle reports the
// shared mode requested by the caller.
func TestSharedLocksAdmitOtherReaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object.lock")
	first, err := flock.Acquire(context.Background(), path, flock.WithMode(flock.Shared))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Release() }()

	second, err := flock.TryAcquire(path, flock.WithMode(flock.Shared))
	if err != nil {
		t.Fatalf("a shared lock excluded a second reader: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	if first.Mode() != flock.Shared {
		t.Fatalf("Mode reported %v", first.Mode())
	}
}

// TestSharedLockExcludesAWriter verifies that an active reader protects its
// snapshot from mutation by an exclusive holder.
func TestSharedLockExcludesAWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object.lock")
	reader, err := flock.Acquire(context.Background(), path, flock.WithMode(flock.Shared))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Release() }()

	if _, err := flock.TryAcquire(path); !errors.Is(err, flock.ErrLocked) {
		t.Fatalf("a shared lock admitted a writer: %v", err)
	}
}

// TestExclusiveLockExcludesAReader verifies the converse reader/writer rule: a
// writer must exclude readers so they cannot observe an in-progress operation.
func TestExclusiveLockExcludesAReader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object.lock")
	writer, err := flock.Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = writer.Release() }()

	_, err = flock.TryAcquire(path, flock.WithMode(flock.Shared))
	if !errors.Is(err, flock.ErrLocked) {
		t.Fatalf("an exclusive lock admitted a reader: %v", err)
	}
}

// A defer beside an explicit release calls Release two times. The second call
// must not act on a descriptor that the first call closed.
func TestReleaseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object.lock")
	lock, err := flock.Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("a second Release returned %v", err)
	}

	// The lock is free after two releases, and not held by a reused descriptor.
	next, err := flock.TryAcquire(path)
	if err != nil {
		t.Fatalf("the lock stayed held: %v", err)
	}
	if err := next.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestAcquireFileLocksADirectory covers caller-owned descriptors and directory
// locks, which cannot use the normal write-open path. Contention must still work,
// Path must identify the descriptor, and Release must unlock without closing a
// file it did not open.
func TestAcquireFileLocksADirectory(t *testing.T) {
	directory := t.TempDir()
	file, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	lock, err := flock.AcquireFile(context.Background(), file)
	if err != nil {
		t.Fatalf("a directory could not be locked: %v", err)
	}
	if lock.Path() != directory {
		t.Fatalf("Path reported %q, want %q", lock.Path(), directory)
	}

	// TryAcquire cannot test this, because it opens for writing and a
	// directory refuses that. A second descriptor is the only way in.
	other, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := flock.AcquireFile(ctx, other); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a locked directory was locked twice: %v", err)
	}

	// Release must leave a file that the caller opened open.
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Stat(); err != nil {
		t.Fatalf("Release closed a file it does not own: %v", err)
	}
}

// TestHoldReleasesAfterWorkFails protects the structured-lock helper's cleanup
// path. The callback's error must be returned unchanged while the lock is always
// released, otherwise one failed DAC operation could deadlock later commands.
func TestHoldReleasesAfterWorkFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object.lock")
	failure := errors.New("work failed")

	err := flock.Hold(context.Background(), path, func(context.Context) error { return failure })
	if !errors.Is(err, failure) {
		t.Fatalf("Hold returned %v, want the error from work", err)
	}

	lock, err := flock.TryAcquire(path)
	if err != nil {
		t.Fatalf("Hold kept the lock after work failed: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestHoldRunsWorkUnderTheLock checks ordering inside the convenience helper:
// work must run after acquisition, not merely adjacent to an acquire/release
// sequence. The callback probes the lock directly to catch that regression.
func TestHoldRunsWorkUnderTheLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object.lock")
	var ran bool
	err := flock.Hold(context.Background(), path, func(context.Context) error {
		ran = true
		if _, err := flock.TryAcquire(path); !errors.Is(err, flock.ErrLocked) {
			return errors.New("work ran without the lock")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("Hold did not run work")
	}
}

// TestWithoutCreateRefusesAMissingFile preserves the read-only acquisition mode.
// A missing target must return fs.ErrNotExist without creating a sidecar as an
// accidental observation side effect.
func TestWithoutCreateRefusesAMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.lock")
	_, err := flock.TryAcquire(path, flock.WithoutCreate())
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file returned %v, want fs.ErrNotExist", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatal("WithoutCreate created the file")
	}
}

// TestErrorsNameThePath keeps contention diagnostics actionable and
// machine-checkable. Wrapping ErrLocked in fs.PathError must add the affected
// path without breaking errors.Is checks used for control flow.
func TestErrorsNameThePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object.lock")
	held, err := flock.Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()

	_, err = flock.TryAcquire(path)
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("TryAcquire returned %T, want *fs.PathError", err)
	}
	if pathErr.Path != path {
		t.Fatalf("the error names %q, want %q", pathErr.Path, path)
	}
	// The wrapper must not hide the sentinel.
	if !errors.Is(err, flock.ErrLocked) {
		t.Fatal("the path error hid ErrLocked")
	}
}

// TestDirModeAppliesToANewDirectory verifies that callers can constrain newly
// created lock directories, which may contain names or metadata not intended for
// other users on the system.
func TestDirModeAppliesToANewDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "locks")
	lock, err := flock.Acquire(context.Background(), filepath.Join(directory, "object.lock"),
		flock.WithDirMode(0o700))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()

	info, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("the directory has mode %v, want 700", info.Mode().Perm())
	}
}

// A caller that waits with its own backoff must still get the lock when the
// holder releases it.
func TestBackoffStillAcquiresAReleasedLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object.lock")
	held, err := flock.Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	released := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		released <- held.Release()
	}()

	lock, err := flock.Acquire(context.Background(), path,
		flock.WithBackoff(time.Millisecond, 5*time.Millisecond))
	if err != nil {
		t.Fatalf("Acquire with a backoff returned %v", err)
	}
	if err := <-released; err != nil {
		t.Fatalf("Release returned %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}

// The lock file is open for writing, so a caller can record what holds it.
func TestFileIsOpenForWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object.lock")
	lock, err := flock.Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()

	if _, err := lock.File().WriteString("held\n"); err != nil {
		t.Fatalf("the lock file is not open for writing: %v", err)
	}
	if got := lock.String(); !strings.Contains(got, "exclusive") || !strings.Contains(got, path) {
		t.Fatalf("String reported %q", got)
	}
}

// TestFileModeAppliesToANewLockFile ensures caller-selected permissions apply to
// lock metadata at creation rather than after a window with broader defaults.
func TestFileModeAppliesToANewLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object.lock")
	lock, err := flock.Acquire(context.Background(), path, flock.WithFileMode(0o640))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Release() }()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("the lock file has mode %v, want 640", info.Mode().Perm())
	}
}
