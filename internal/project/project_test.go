package project

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tom/dac/internal/coord"
	"github.com/tom/dac/internal/digest"
)

func TestWritePairRoundTrip(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	manifest := Manifest{
		SchemaVersion: ManifestVersion,
		Assets: map[coord.Coordinate]Asset{
			coord.MustParse("app/geo@2026.08"): {URL: "https://example.com/geo.bin"},
		},
	}
	lock, err := NewLock(manifest, map[coord.Coordinate]LockAsset{
		coord.MustParse("app/geo@2026.08"): {URL: "https://example.com/geo.bin", Digest: digest.Bytes([]byte("geo")), Size: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WritePair(manifestPath, lockPath, manifest, lock); err != nil {
		t.Fatal(err)
	}

	readManifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	readLock, err := ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckLock(readManifest, readLock); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	// The version lives in the key. A "version" field beside it would be a
	// second place to say the same thing, and the place the two could disagree.
	if strings.Contains(string(data), "\"version\"") {
		t.Fatalf("lock repeats the version outside the key: %s", data)
	}
	if !strings.Contains(string(data), "\"app/geo@2026.08\"") {
		t.Fatalf("lock is not keyed by coordinate: %s", data)
	}
}

func TestReadManifestRejectsDuplicateKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dac.json")
	data := []byte("{\"schemaVersion\":2,\"assets\":{},\"assets\":{}}")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(path); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate-key error, got %v", err)
	}
}

// A key that DAC would refuse to read back must not reach a file, so validation
// covers a manifest built in memory as well as one that was parsed.
func TestManifestRejectsInvalidCoordinates(t *testing.T) {
	for _, name := range []coord.Coordinate{
		{Name: "geo", Version: "1"},
		{Namespace: "app", Version: "1"},
		{Namespace: "app", Name: "geo"},
		{Namespace: "App", Name: "geo", Version: "1"},
		{Namespace: "app", Name: "-geo", Version: "1"},
	} {
		manifest := Manifest{
			SchemaVersion: ManifestVersion,
			Assets:        map[coord.Coordinate]Asset{name: {URL: "https://example.com/a"}},
		}
		if err := manifest.Validate(); err == nil {
			t.Fatalf("expected %#v to fail validation", name)
		}
	}
}

// A manifest written before versions were part of the key is not a manifest
// this DAC can read, and saying so is better than guessing at what it meant.
func TestReadManifestRejectsTheOlderSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dac.json")
	data := []byte(`{"schemaVersion":1,"assets":{"geo":{"version":"1","url":"https://example.com/a"}}}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(path); err == nil {
		t.Fatal("expected the older schema to be rejected")
	}
}

func TestCheckLockRejectsStaleManifest(t *testing.T) {
	manifest, lock := Empty()
	manifest.Assets[coord.MustParse("app/asset@1")] = Asset{URL: "https://example.com/a"}
	if err := CheckLock(manifest, lock); err == nil {
		t.Fatal("expected a stale lock error")
	}
}

// The invariant that keeps a version meaningful, enforced where a Lock value is
// built rather than only where one is written.
func TestLockRejectsTwoVersionsOfTheSameBytes(t *testing.T) {
	value := digest.Bytes([]byte("bytes"))
	manifest := Manifest{
		SchemaVersion: ManifestVersion,
		Assets: map[coord.Coordinate]Asset{
			coord.MustParse("app/geo@1"): {URL: "https://example.com/one"},
			coord.MustParse("app/geo@2"): {URL: "https://example.com/two"},
		},
	}
	_, err := NewLock(manifest, map[coord.Coordinate]LockAsset{
		coord.MustParse("app/geo@1"): {URL: "https://example.com/one", Digest: value, Size: 5},
		coord.MustParse("app/geo@2"): {URL: "https://example.com/two", Digest: value, Size: 5},
	})
	var collision *VersionCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("expected a version collision, got %v", err)
	}
	if collision.Group.String() != "app/geo" || len(collision.Versions) != 2 {
		t.Fatalf("unexpected collision: %#v", collision)
	}
}

// Two versions of two different assets naming one object is not a collision.
// The cache stores it once, and a namespace exists so that two projects can
// vendor the same file without either one owning it.
func TestLockAllowsTwoAssetsThatShareAnObject(t *testing.T) {
	value := digest.Bytes([]byte("bytes"))
	manifest := Manifest{
		SchemaVersion: ManifestVersion,
		Assets: map[coord.Coordinate]Asset{
			coord.MustParse("backend/geo@1"):  {URL: "https://example.com/geo"},
			coord.MustParse("frontend/geo@1"): {URL: "https://example.com/geo"},
		},
	}
	if _, err := NewLock(manifest, map[coord.Coordinate]LockAsset{
		coord.MustParse("backend/geo@1"):  {URL: "https://example.com/geo", Digest: value, Size: 5},
		coord.MustParse("frontend/geo@1"): {URL: "https://example.com/geo", Digest: value, Size: 5},
	}); err != nil {
		t.Fatal(err)
	}
}
