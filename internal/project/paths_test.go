package project

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenDownloadsRejectsAnEscapingSymlink enforces the project sandbox. An
// attacker-controlled .dac/downloads symlink must not redirect writes outside
// the project, and rejecting it must leave the external directory untouched.
func TestOpenDownloadsRejectsAnEscapingSymlink(t *testing.T) {
	directory := t.TempDir()
	outside := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, ".dac"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(directory, ".dac", "downloads")); err != nil {
		t.Fatal(err)
	}
	if downloads, err := (Paths{Root: directory}).OpenDownloads(); err == nil {
		_ = downloads.Close()
		t.Fatal("OpenDownloads followed a symlink outside the project")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("outside directory was modified: %v", entries)
	}
}
