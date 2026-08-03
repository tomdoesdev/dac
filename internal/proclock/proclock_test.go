package proclock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLockExcludesAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object.lock")
	first, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	// A second acquisition must wait, so a bounded context must expire.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a held lock was acquired twice: %v", err)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(context.Background(), path)
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
			lock, err := Acquire(context.Background(), path)
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
		t.Fatalf("%d processes held the lock at once", got)
	}
}

func TestAcquireCreatesMissingDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "locks", "nested", "object.lock")
	lock, err := Acquire(context.Background(), path)
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
	held, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Acquire(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled acquisition returned %v", err)
	}
}
