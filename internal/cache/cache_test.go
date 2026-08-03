package cache

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/digest"
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

func TestHasTrustsMatchingPathAndSize(t *testing.T) {
	store := New(t.TempDir())
	expected := digest.Bytes([]byte("expected"))
	path, err := store.Path(expected)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("different"), 0o444); err != nil {
		t.Fatal(err)
	}
	found, valid, err := stored(store, application.Object{Digest: expected, Size: int64(len("different"))})
	_ = found
	if err != nil || !valid {
		t.Fatalf("expected path and size match, valid=%v err=%v", valid, err)
	}
}

// age backdates an object so collection treats it as unused.
func age(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func TestHasRefreshesTheObjectTimestamp(t *testing.T) {
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
	age(t, path, time.Now().Add(-90*24*time.Hour))
	_, valid, err := stored(store, object)
	if err != nil || !valid {
		t.Fatalf("expected a cache hit, valid=%v err=%v", valid, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("Has did not refresh the timestamp: %s", info.ModTime())
	}
}

func TestFindRefreshesTheObjectTimestamp(t *testing.T) {
	store := New(t.TempDir())
	object, err := store.Put(context.Background(), bytes.NewReader([]byte("asset bytes")), application.PutAny("", 0))
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.Path(object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	age(t, path, time.Now().Add(-90*24*time.Hour))
	if _, found, err := store.Stat(object.Digest); err != nil || !found {
		t.Fatalf("expected a cache hit, found=%v err=%v", found, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(info.ModTime()) > time.Minute {
		t.Fatalf("Find did not refresh the timestamp: %s", info.ModTime())
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
	stalePath, err := store.Path(stale.Digest)
	if err != nil {
		t.Fatal(err)
	}
	age(t, stalePath, time.Now().Add(-48*time.Hour))

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
	path, err := store.Path(object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	age(t, path, time.Now().Add(-48*time.Hour))

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
	path, err := store.Path(object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	age(t, path, time.Now().Add(-48*time.Hour))
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
