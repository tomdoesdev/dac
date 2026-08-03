package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tom/dac/internal/digest"
)

func TestWritePairRoundTrip(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	manifest := Manifest{
		SchemaVersion: ManifestVersion,
		Assets: map[string]Asset{
			"geo": {Version: "2026.08", URL: "https://example.com/geo.bin"},
		},
	}
	lock, err := NewLock(manifest, map[string]LockAsset{
		"geo": {Version: "2026.08", URL: "https://example.com/geo.bin", Digest: digest.Bytes([]byte("geo")), Size: 3},
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
	if strings.Contains(string(data), "descriptor") || strings.Contains(string(data), "mediaType") {
		t.Fatalf("lock contains removed fields: %s", data)
	}
}

func TestReadManifestRejectsDuplicateKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dac.json")
	data := []byte("{\"schemaVersion\":1,\"assets\":{},\"assets\":{}}")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(path); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate-key error, got %v", err)
	}
}

func TestManifestRejectsInvalidCoordinates(t *testing.T) {
	for name, asset := range map[string]Asset{
		"":      {Version: "1", URL: "https://example.com/a"},
		"bad@x": {Version: "1", URL: "https://example.com/a"},
		"bad":   {Version: "1@2", URL: "https://example.com/a"},
	} {
		manifest := Manifest{SchemaVersion: ManifestVersion, Assets: map[string]Asset{name: asset}}
		if err := manifest.Validate(); err == nil {
			t.Fatalf("expected %q to fail validation", name)
		}
	}
}

func TestCheckLockRejectsStaleManifest(t *testing.T) {
	manifest, lock := Empty()
	manifest.Assets["asset"] = Asset{Version: "1", URL: "https://example.com/a"}
	if err := CheckLock(manifest, lock); err == nil {
		t.Fatal("expected a stale lock error")
	}
}
