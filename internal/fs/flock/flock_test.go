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

	"github.com/tomdoesdev/dac/internal/fs/flock"
)

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
