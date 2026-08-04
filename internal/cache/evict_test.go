package cache

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/application"
)

// TestGCEvictsTheLeastRecentlyUsedUntilItFits covers the question age cannot
// answer. A machine whose projects genuinely use more than the disk it has to
// spare has nothing old enough to collect and is still too full.
func TestGCEvictsTheLeastRecentlyUsedUntilItFits(t *testing.T) {
	store := New(t.TempDir())
	oldest := put(t, store, strings.Repeat("a", 400))
	middle := put(t, store, strings.Repeat("b", 400))
	newest := put(t, store, strings.Repeat("c", 400))
	// All three are live. Only the order they were last reached in decides.
	ageUse(t, store, oldest.Digest, time.Now().Add(-3*time.Hour))
	ageUse(t, store, middle.Digest, time.Now().Add(-2*time.Hour))
	ageUse(t, store, newest.Digest, time.Now().Add(-time.Hour))

	// Room for one object, so the two oldest go.
	result, err := store.GC(context.Background(), application.GCOptions{MaxAge: 24 * time.Hour, MaxSize: 500})
	if err != nil {
		t.Fatal(err)
	}
	if result.EvictedCount != 2 || result.EvictedBytes != oldest.Size+middle.Size {
		t.Fatalf("unexpected eviction: %#v", result)
	}
	// Eviction is part of the removal rather than beside it, so the totals
	// count it.
	if result.ObjectCount != 2 || result.ByteCount != result.EvictedBytes {
		t.Fatalf("evicted objects missing from the totals: %#v", result)
	}
	want := []string{oldest.Digest, middle.Digest}
	slices.Sort(want)
	if !slices.Equal(result.Digests, want) {
		t.Fatalf("evicted %v, want the two least recently used %v", result.Digests, want)
	}
	if _, valid, err := stored(store, newest); err != nil || !valid {
		t.Fatalf("the most recently used object was evicted, valid=%v err=%v", valid, err)
	}
}

// TestGCLeavesACacheInsideItsBoundAlone keeps a bound from being a reason to
// delete anything on its own.
func TestGCLeavesACacheInsideItsBoundAlone(t *testing.T) {
	store := New(t.TempDir())
	object := put(t, store, "a small asset")

	result, err := store.GC(context.Background(), application.GCOptions{MaxAge: 24 * time.Hour, MaxSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if result.ObjectCount != 0 || result.EvictedCount != 0 {
		t.Fatalf("a cache inside its bound lost objects: %#v", result)
	}
	if _, valid, err := stored(store, object); err != nil || !valid {
		t.Fatalf("the object was removed, valid=%v err=%v", valid, err)
	}
}

// TestGCWithoutASizeBoundEvictsNothing is the behaviour every existing caller
// has. Zero is no bound, which is what collection by age alone amounts to.
func TestGCWithoutASizeBoundEvictsNothing(t *testing.T) {
	store := New(t.TempDir())
	object := put(t, store, strings.Repeat("a", 4096))

	result, err := store.GC(context.Background(), application.GCOptions{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if result.EvictedCount != 0 {
		t.Fatalf("an unbounded cache evicted: %#v", result)
	}
	if _, valid, err := stored(store, object); err != nil || !valid {
		t.Fatalf("the object was removed, valid=%v err=%v", valid, err)
	}
}

// TestGCCollectsByAgeBeforeEvicting covers the order the two passes run in.
// Taking what nothing is using costs nobody a download, so it happens first and
// may leave nothing for the bound to do.
func TestGCCollectsByAgeBeforeEvicting(t *testing.T) {
	store := New(t.TempDir())
	stale := put(t, store, strings.Repeat("a", 400))
	live := put(t, store, strings.Repeat("b", 400))
	ageUse(t, store, stale.Digest, time.Now().Add(-48*time.Hour))

	// The bound alone would have taken one object. Collection takes the stale
	// one first, which brings the cache under without touching the live one.
	result, err := store.GC(context.Background(), application.GCOptions{MaxAge: 24 * time.Hour, MaxSize: 500})
	if err != nil {
		t.Fatal(err)
	}
	if result.ObjectCount != 1 || result.EvictedCount != 0 {
		t.Fatalf("collection and eviction overlapped: %#v", result)
	}
	if _, valid, err := stored(store, live); err != nil || !valid {
		t.Fatalf("the live object was evicted anyway, valid=%v err=%v", valid, err)
	}
}

// TestGCDryRunEvictsNothingAndCountsOnce covers the careful path, and the one
// way it could double-count: on a dry run the objects collection would take are
// still on disk, so a size pass that counted them would report evicting what it
// has already said it would collect.
func TestGCDryRunEvictsNothingAndCountsOnce(t *testing.T) {
	store := New(t.TempDir())
	stale := put(t, store, strings.Repeat("a", 400))
	live := put(t, store, strings.Repeat("b", 400))
	ageUse(t, store, stale.Digest, time.Now().Add(-48*time.Hour))

	result, err := store.GC(context.Background(), application.GCOptions{MaxAge: 24 * time.Hour, MaxSize: 500, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.ObjectCount != 1 || result.EvictedCount != 0 || len(result.Digests) != 1 {
		t.Fatalf("a dry run counted the stale object twice: %#v", result)
	}
	for _, object := range []application.Object{stale, live} {
		if _, valid, err := stored(store, object); err != nil || !valid {
			t.Fatalf("a dry run removed an object, valid=%v err=%v", valid, err)
		}
	}
}

// TestGCClearIgnoresTheSizeBound covers the one collection a bound has nothing
// to say about. Clearing takes everything, so there is no cache left to be over.
func TestGCClearIgnoresTheSizeBound(t *testing.T) {
	store := New(t.TempDir())
	put(t, store, strings.Repeat("a", 400))
	put(t, store, strings.Repeat("b", 400))

	result, err := store.GC(context.Background(), application.GCOptions{MaxSize: 500, All: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.ObjectCount != 2 || result.EvictedCount != 0 {
		t.Fatalf("clearing reported an eviction: %#v", result)
	}
}

// TestGCDoesNotEvictAnObjectUsedWhileItWaited covers the re-check under the
// digest lock. Waiting for that lock takes time, and an object something
// reached for meanwhile is the last one worth taking: it has just become the
// most recently used thing in the cache rather than the least.
func TestGCDoesNotEvictAnObjectUsedWhileItWaited(t *testing.T) {
	store := New(t.TempDir())
	guarded := put(t, store, strings.Repeat("a", 400))
	other := put(t, store, strings.Repeat("b", 400))
	// The oldest, so eviction reaches for it first.
	ageUse(t, store, guarded.Digest, time.Now().Add(-3*time.Hour))
	ageUse(t, store, other.Digest, time.Now().Add(-2*time.Hour))

	type outcome struct {
		result application.GCResult
		err    error
	}
	done := make(chan outcome, 1)
	err := store.WithLock(context.Background(), guarded.Digest, func() error {
		go func() {
			result, err := store.GC(context.Background(), application.GCOptions{MaxAge: 24 * time.Hour, MaxSize: 500})
			done <- outcome{result: result, err: err}
		}()
		// The collection has read every timestamp by now and is blocked on the
		// lock this holds. Stat refreshes the sidecar, which is what another
		// process reaching for the object looks like, and it needs no lock --
		// so it lands squarely inside the window the re-check exists for.
		select {
		case finished := <-done:
			t.Errorf("the collection evicted without the lock: %#v", finished.result)
		case <-time.After(250 * time.Millisecond):
		}
		if _, _, err := store.Stat(guarded.Digest); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	finished := <-done
	if finished.err != nil {
		t.Fatal(finished.err)
	}
	// The object it decided on is now the most recently used thing in the
	// cache rather than the least, so it is the last one worth taking.
	if _, valid, err := stored(store, guarded); err != nil || !valid {
		t.Fatalf("an object used while the collection waited was evicted, valid=%v err=%v", valid, err)
	}
	if slices.Contains(finished.result.Digests, guarded.Digest) {
		t.Fatalf("the guarded object was reported as removed: %#v", finished.result)
	}
}

// put stores bytes and returns the object, which every test here starts with.
func put(t *testing.T, store *Store, content string) application.Object {
	t.Helper()
	object, err := store.Put(context.Background(), bytes.NewReader([]byte(content)), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	return object
}
