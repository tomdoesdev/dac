package cache

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/jsonfile"
)

func TestPutAndLookupObject(t *testing.T) {
	store := New(t.TempDir())
	content := []byte("asset bytes")
	expected := digest.Bytes(content)
	object, err := store.Put(context.Background(), bytes.NewReader(content), application.PutAny(expected, 0))
	if err != nil {
		t.Fatal(err)
	}
	if object.Digest != expected || object.Size != int64(len(content)) {
		t.Fatalf("unexpected object: %#v", object)
	}
	found, ok, err := store.Stat(expected)
	if err != nil || !ok || found != object {
		t.Fatalf("unexpected lookup: %#v %v %v", found, ok, err)
	}
	path, err := store.Path(expected)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("object mode is %o", info.Mode().Perm())
	}
}

func TestPutRejectsDigestAndSizeMismatch(t *testing.T) {
	store := New(t.TempDir())
	content := []byte("asset bytes")
	if _, err := store.Put(context.Background(), bytes.NewReader(content), application.PutAny(digest.Bytes([]byte("other")), 0)); err == nil {
		t.Fatal("expected a digest mismatch")
	}
	short := application.Object{Digest: digest.Bytes(content), Size: int64(len(content) - 1)}
	if _, err := store.Put(context.Background(), bytes.NewReader(content), application.PutExact(short)); err == nil {
		t.Fatal("expected a size mismatch")
	}
}

func TestPutEnforcesUnknownSizeLimit(t *testing.T) {
	store := New(t.TempDir())
	_, err := store.Put(context.Background(), bytes.NewReader([]byte("too large")), application.PutAny("", 3))
	if !errors.Is(err, application.ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
}

func TestPutHonorsCancellation(t *testing.T) {
	store := New(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Put(ctx, bytes.NewReader([]byte("asset")), application.PutAny("", 0)); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

// place writes content at the path a digest names, whether or not the content
// actually hashes to it. It is how a test produces a cache the store has to
// catch: a name that no longer describes its bytes.
func place(t *testing.T, store *Store, value string, content []byte) string {
	t.Helper()
	path, err := store.Path(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o444); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStatRejectsAnObjectThatDoesNotMatchItsDigest(t *testing.T) {
	store := New(t.TempDir())
	expected := digest.Bytes([]byte("expected"))
	place(t, store, expected, []byte("different"))

	_, _, err := store.Stat(expected)
	var corrupt *application.CorruptError
	if !errors.As(err, &corrupt) {
		t.Fatalf("expected a CorruptError, got %v", err)
	}
	if corrupt.Digest != expected || corrupt.ActualDigest != digest.Bytes([]byte("different")) {
		t.Fatalf("unexpected corruption report: %#v", corrupt)
	}
}

// TestStatDetectsCorruptionThatPreservesTheSize is the case the old store could
// not see: bytes replaced in place by exactly as many bytes.
func TestStatDetectsCorruptionThatPreservesTheSize(t *testing.T) {
	store := New(t.TempDir())
	content := []byte("asset bytes")
	object, err := store.Put(context.Background(), bytes.NewReader(content), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.Path(object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh store has no memory of this object, so the check has to come from
	// the sidecar rather than from what this process already hashed.
	store = New(store.Root)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("ASSET BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Stat(object.Digest); !errors.As(err, new(*application.CorruptError)) {
		t.Fatalf("same-size corruption went undetected: %v", err)
	}
}

func TestStatHealsAnObjectWithNoSidecar(t *testing.T) {
	store := New(t.TempDir())
	content := []byte("asset bytes")
	value := digest.Bytes(content)
	path := place(t, store, value, content)

	// A cache DAC wrote before the sidecar format has objects but no sidecars.
	// The first read hashes one and records what it found.
	found, ok, err := store.Stat(value)
	if err != nil || !ok || found.Size != int64(len(content)) {
		t.Fatalf("unexpected lookup: %#v %v %v", found, ok, err)
	}
	if _, err := os.Stat(metaPath(path)); err != nil {
		t.Fatalf("the sidecar was not written: %v", err)
	}
}

func TestPutRepairsACorruptObject(t *testing.T) {
	store := New(t.TempDir())
	content := []byte("asset bytes")
	value := digest.Bytes(content)
	place(t, store, value, []byte("ASSET BYTES"))

	if _, err := store.Put(context.Background(), bytes.NewReader(content), application.PutAny(value, 0)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Stat(value); err != nil {
		t.Fatalf("the object was not repaired: %v", err)
	}
}

func TestVerifyIgnoresTheSidecar(t *testing.T) {
	store := New(t.TempDir())
	content := []byte("asset bytes")
	object, err := store.Put(context.Background(), bytes.NewReader(content), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.Path(object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	// Damage the object and restore the stat the sidecar recorded, which is the
	// one case the cheap check is blind to and the whole reason Verify exists.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("ASSET BYTES"), 0o644); err != nil {
		t.Fatal(err)
	}
	age(t, path, info.ModTime())

	fresh := New(store.Root)
	if _, _, err := fresh.Stat(object.Digest); err != nil {
		t.Fatalf("the cheap check was expected to miss this, got %v", err)
	}
	if _, found, err := fresh.Verify(context.Background(), object.Digest); !found || !errors.As(err, new(*application.CorruptError)) {
		t.Fatalf("Verify missed the damage: found=%v err=%v", found, err)
	}
}

func TestRemoveDeletesTheObjectAndItsSidecar(t *testing.T) {
	store := New(t.TempDir())
	object, err := store.Put(context.Background(), bytes.NewReader([]byte("asset bytes")), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.Path(object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Remove(context.Background(), object.Digest); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{path, metaPath(path)} {
		if _, err := os.Stat(removed); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived removal: %v", removed, err)
		}
	}
}

func TestListReturnsObjectsWithoutSidecars(t *testing.T) {
	store := New(t.TempDir())
	first, err := store.Put(context.Background(), bytes.NewReader([]byte("one")), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Put(context.Background(), bytes.NewReader([]byte("two")), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	digests, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{first.Digest, second.Digest}
	slices.Sort(want)
	if !slices.Equal(digests, want) {
		t.Fatalf("unexpected listing: %v, want %v", digests, want)
	}
}

// age backdates a file.
func age(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// ageUse backdates an object's sidecar so collection treats it as unused. The
// sidecar carries the liveness signal, so this is what "no project has
// referenced this object in a long time" looks like on disk.
func ageUse(t *testing.T, store *Store, value string, when time.Time) {
	t.Helper()
	path, err := store.Path(value)
	if err != nil {
		t.Fatal(err)
	}
	age(t, metaPath(path), when)
}

func TestStatRefreshesTheSidecarAndNotTheObject(t *testing.T) {
	store := New(t.TempDir())
	object, err := store.Put(context.Background(), bytes.NewReader([]byte("asset bytes")), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.Path(object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	ageUse(t, store, object.Digest, time.Now().Add(-90*24*time.Hour))

	if _, found, err := store.Stat(object.Digest); err != nil || !found {
		t.Fatalf("expected a cache hit, found=%v err=%v", found, err)
	}
	sidecar, err := os.Stat(metaPath(path))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(sidecar.ModTime()) > time.Minute {
		t.Fatalf("the cache hit did not refresh the sidecar: %s", sidecar.ModTime())
	}
	// The object's own timestamp is the evidence that nothing has written to it,
	// so a read must leave it exactly where the install put it.
	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !current.ModTime().Equal(installed.ModTime()) {
		t.Fatalf("a cache hit moved the object timestamp: %s became %s", installed.ModTime(), current.ModTime())
	}
}

func TestGCRemovesOnlyObjectsOlderThanMaxAge(t *testing.T) {
	store := New(t.TempDir())
	stale, err := store.Put(context.Background(), bytes.NewReader([]byte("stale bytes")), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := store.Put(context.Background(), bytes.NewReader([]byte("fresh bytes")), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	ageUse(t, store, stale.Digest, time.Now().Add(-48*time.Hour))

	result, err := store.GC(context.Background(), application.GCOptions{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if result.ObjectCount != 1 || len(result.Digests) != 1 || result.Digests[0] != stale.Digest {
		t.Fatalf("unexpected collection: %#v", result)
	}
	if result.ByteCount != stale.Size {
		t.Fatalf("collection freed %d bytes, want %d", result.ByteCount, stale.Size)
	}
	if _, valid, err := stored(store, stale); err != nil || valid {
		t.Fatalf("the stale object survived, valid=%v err=%v", valid, err)
	}
	if _, valid, err := stored(store, fresh); err != nil || !valid {
		t.Fatalf("the fresh object was removed, valid=%v err=%v", valid, err)
	}
}

func TestGCDryRunKeepsEverything(t *testing.T) {
	store := New(t.TempDir())
	object, err := store.Put(context.Background(), bytes.NewReader([]byte("asset bytes")), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	ageUse(t, store, object.Digest, time.Now().Add(-48*time.Hour))

	result, err := store.GC(context.Background(), application.GCOptions{MaxAge: time.Hour, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.ObjectCount != 1 || !result.DryRun {
		t.Fatalf("unexpected collection: %#v", result)
	}
	if _, valid, err := stored(store, object); err != nil || !valid {
		t.Fatalf("a dry run removed the object, valid=%v err=%v", valid, err)
	}
}

// TestVerifyStopsWhenTheCommandDoes covers the context Verify takes. A scrub
// hashes every object in a cache in full, and an object big enough to be worth
// caching is big enough that somebody may want to stop waiting for it.
func TestVerifyStopsWhenTheCommandDoes(t *testing.T) {
	store := New(t.TempDir())
	object, err := store.Put(context.Background(), bytes.NewReader([]byte("asset bytes")), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.Verify(ctx, object.Digest); !errors.Is(err, context.Canceled) {
		t.Fatalf("verify error = %v, want context.Canceled", err)
	}
	// A cancelled check settles nothing, so the object is still there to check later.
	if _, found, err := store.Stat(object.Digest); err != nil || !found {
		t.Fatalf("cancelling a check disturbed the object: found=%v err=%v", found, err)
	}
}

// TestClearingLeavesATemporaryFileInFlight covers the one thing a collection does
// not get to take on its cutoff alone. Clearing removes every object by design,
// but a download in flight is an open temporary file in a directory this sweep
// walks, and an age says nothing about whether somebody is still writing to it.
func TestClearingLeavesATemporaryFileInFlight(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	// An object gives the sidecar sweep a directory to walk, which is where the other kind of temporary file lands.
	if _, err := store.Put(context.Background(), bytes.NewReader([]byte("asset bytes")), application.PutAny("", 0)); err != nil {
		t.Fatal(err)
	}
	downloads := filepath.Join(root, "tmp")
	if err := os.MkdirAll(downloads, 0o755); err != nil {
		t.Fatal(err)
	}
	blobs := filepath.Join(root, "blobs", "sha256")
	running := filepath.Join(downloads, "download-running")
	abandoned := filepath.Join(downloads, "download-abandoned")
	writing := filepath.Join(blobs, jsonfile.TempPrefix+"writing")
	for _, path := range []string{running, abandoned, writing} {
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	age(t, abandoned, time.Now().Add(-48*time.Hour))

	result, err := store.GC(context.Background(), application.GCOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.TempCount != 1 {
		t.Fatalf("clearing removed %d temporary files, want 1", result.TempCount)
	}
	for _, path := range []string{running, writing} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("clearing took %s, which something may still be writing: %v", filepath.Base(path), err)
		}
	}
	if _, err := os.Stat(abandoned); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the abandoned download survived a clear")
	}
}

func TestGCRemovesAbandonedTemporaryFiles(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	directory := filepath.Join(root, "tmp")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(directory, "download-stale")
	fresh := filepath.Join(directory, "download-fresh")
	other := filepath.Join(directory, "unrelated")
	for _, path := range []string{stale, fresh, other} {
		if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	age(t, stale, time.Now().Add(-48*time.Hour))
	age(t, other, time.Now().Add(-48*time.Hour))

	result, err := store.GC(context.Background(), application.GCOptions{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if result.TempCount != 1 {
		t.Fatalf("collection removed %d temporary files, want 1", result.TempCount)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the abandoned download survived")
	}
	for _, path := range []string{fresh, other} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was removed: %v", path, err)
		}
	}
}

func TestGCKeepsDigestLockFiles(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	object, err := store.Put(context.Background(), bytes.NewReader([]byte("asset bytes")), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	ageUse(t, store, object.Digest, time.Now().Add(-48*time.Hour))
	locks := filepath.Join(root, "locks")
	before, err := os.ReadDir(locks)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("the test did not create a lock file")
	}
	if _, err := store.GC(context.Background(), application.GCOptions{MaxAge: time.Hour}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(locks)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("collection removed lock files: %d became %d", len(before), len(after))
	}
}

func TestGCOnAnEmptyCacheSucceeds(t *testing.T) {
	result, err := New(t.TempDir()).GC(context.Background(), application.GCOptions{MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if result.ObjectCount != 0 || len(result.Digests) != 0 {
		t.Fatalf("unexpected collection: %#v", result)
	}
}

func TestGCRejectsANegativeMaxAge(t *testing.T) {
	if _, err := New(t.TempDir()).GC(context.Background(), application.GCOptions{MaxAge: -time.Hour}); err == nil {
		t.Fatal("a negative age was accepted")
	}
}

// stored reports whether the cache holds exactly the expected object, which is
// what every caller of Stat actually wants to know.
func stored(store *Store, object application.Object) (application.Object, bool, error) {
	found, exists, err := store.Stat(object.Digest)
	return found, exists && found.Size == object.Size, err
}

func TestGCRemovesOrphanedSidecars(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	object, err := store.Put(context.Background(), bytes.NewReader([]byte("asset bytes")), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.Path(object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	// Collection removes the pair together, so a sidecar that survives alone
	// describes an object something outside DAC deleted.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	age(t, metaPath(path), time.Now().Add(-48*time.Hour))
	result, err := store.GC(context.Background(), application.GCOptions{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if result.SidecarCount != 1 {
		t.Fatalf("collection removed %d orphaned sidecars, want 1", result.SidecarCount)
	}
	if _, err := os.Stat(metaPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the orphaned sidecar survived: %v", err)
	}
}

// TestGCWaitsForTheDigestLockBeforeTakingASidecar covers the window between the
// sweep deciding a sidecar is orphaned and removing it. Every other removal in
// this store happens under the object's lock, and this one used to be the
// exception: an install that landed in that window made the sidecar a current
// record, and taking it anyway cost the next command a full re-hash of an object
// nothing had been in doubt about.
func TestGCWaitsForTheDigestLockBeforeTakingASidecar(t *testing.T) {
	store := New(t.TempDir())
	object, err := store.Put(context.Background(), bytes.NewReader([]byte("asset bytes")), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.Path(object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	age(t, metaPath(path), time.Now().Add(-48*time.Hour))

	type outcome struct {
		result application.GCResult
		err    error
	}
	done := make(chan outcome, 1)
	var early outcome
	finishedEarly := false
	err = store.WithLock(context.Background(), object.Digest, func() error {
		go func() {
			result, err := store.GC(context.Background(), application.GCOptions{MaxAge: 24 * time.Hour})
			done <- outcome{result: result, err: err}
		}()
		// Nothing else in this collection needs the lock, so a sweep that
		// finishes while another process holds it finished without it.
		select {
		case early = <-done:
			finishedEarly = true
		case <-time.After(250 * time.Millisecond):
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if finishedEarly {
		t.Fatalf("the sweep completed while the object's lock was held: %#v", early.result)
	}
	finished := <-done
	if finished.err != nil {
		t.Fatal(finished.err)
	}
	if finished.result.SidecarCount != 1 {
		t.Fatalf("the sweep removed %d orphaned sidecars once the lock was free, want 1", finished.result.SidecarCount)
	}
}

// TestGCKeepsAnOrphanedSidecarYoungerThanTheCutoff ages a sidecar the same way
// the sweep ages everything else it removes.
//
// The sweep walks a listing taken before any of the collection, and it holds no
// lock while it decides. A sidecar that appeared after that listing belongs to
// an object another process is installing right now, and removing it costs the
// next command a full re-hash of an object nothing was in doubt about. Nothing
// this new is anybody's leftover, which is already why the temporary-file
// branch beside it checks the same thing.
func TestGCKeepsAnOrphanedSidecarYoungerThanTheCutoff(t *testing.T) {
	store := New(t.TempDir())
	object, err := store.Put(context.Background(), bytes.NewReader([]byte("asset bytes")), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.Path(object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	result, err := store.GC(context.Background(), application.GCOptions{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if result.SidecarCount != 0 {
		t.Fatalf("collection took %d sidecars younger than the cutoff", result.SidecarCount)
	}
	if _, err := os.Stat(metaPath(path)); err != nil {
		t.Fatalf("a sidecar younger than the cutoff was removed: %v", err)
	}
}

func TestGCKeepsSidecarsThatStillHaveObjects(t *testing.T) {
	store := New(t.TempDir())
	object, err := store.Put(context.Background(), bytes.NewReader([]byte("asset bytes")), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.GC(context.Background(), application.GCOptions{MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if result.SidecarCount != 0 || result.ObjectCount != 0 {
		t.Fatalf("unexpected collection: %#v", result)
	}
	if _, _, err := store.Stat(object.Digest); err != nil {
		t.Fatalf("the object did not survive: %v", err)
	}
}

// TestGCDoesNotCountCollectedSidecarsAsOrphans pins the difference between the
// two ways a sidecar loses its object. Collection removes the pair, and the
// sweep that follows reads a directory listing taken before it did, so a
// collected object's sidecar looks exactly like one that was already alone.
// Counting it reported damage outside DAC for every object DAC itself removed.
func TestGCDoesNotCountCollectedSidecarsAsOrphans(t *testing.T) {
	store := New(t.TempDir())
	object, err := store.Put(context.Background(), bytes.NewReader([]byte("stale bytes")), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	ageUse(t, store, object.Digest, time.Now().Add(-48*time.Hour))

	result, err := store.GC(context.Background(), application.GCOptions{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if result.ObjectCount != 1 {
		t.Fatalf("collection removed %d objects, want 1", result.ObjectCount)
	}
	if result.SidecarCount != 0 {
		t.Fatalf("collecting one object reported %d orphaned sidecars, want 0", result.SidecarCount)
	}
	path, err := store.Path(object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(metaPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the collected object's sidecar survived: %v", err)
	}
}

func TestGCRemovesAbandonedSidecarWrites(t *testing.T) {
	root := t.TempDir()
	store := New(root)
	if _, err := store.Put(context.Background(), bytes.NewReader([]byte("asset bytes")), application.PutAny("", 0)); err != nil {
		t.Fatal(err)
	}
	// A sidecar write that dies between its temporary file and the rename leaves
	// one of these beside the objects, where the download sweep does not look.
	blobs := filepath.Join(root, "blobs", "sha256")
	stale := filepath.Join(blobs, jsonfile.TempPrefix+"stale")
	fresh := filepath.Join(blobs, jsonfile.TempPrefix+"fresh")
	for _, path := range []string{stale, fresh} {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	age(t, stale, time.Now().Add(-48*time.Hour))

	result, err := store.GC(context.Background(), application.GCOptions{MaxAge: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if result.TempCount != 1 {
		t.Fatalf("collection removed %d temporary files, want 1", result.TempCount)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the abandoned sidecar write survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("a recent temporary file was removed: %v", err)
	}
}
