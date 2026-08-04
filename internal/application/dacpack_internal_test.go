package application

// The derivation and the containment check are the two things standing between
// a name a remote server chose and a path DAC writes to. Nothing reachable can
// get past the first, which is exactly why the second is worth testing
// directly: a check no test can trigger through the front door is a check
// nobody finds out has stopped working.

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tom/dac/internal/coord"
	"github.com/tom/dac/internal/project"
)

func TestPackFilePathDerivesFromTheCoordinate(t *testing.T) {
	name := coord.MustParse("java/sdk@11")
	path, err := packFilePath(name, "jdk-11.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if path != "assets/java/sdk/11/jdk-11.tar.gz" {
		t.Fatalf("packFilePath returned %q", path)
	}
	// The file name is the only part that arrives from outside, so it is the
	// only part that can be refused here.
	for _, file := range []string{
		"", "   ", ".", "..", "../etc/passwd", "a/b", `a\b`,
		"-rf", "with\x00null", "with\nnewline", strings.Repeat("a", 256),
	} {
		if path, err := packFilePath(name, file); err == nil {
			t.Errorf("packFilePath accepted %q as %q", file, path)
		}
	}
}

func TestPackFileNameFallsBackToTheCoordinate(t *testing.T) {
	name := coord.MustParse("app/geo@1")
	for _, testCase := range []struct {
		recorded string
		want     string
	}{
		{"database.bin", "database.bin"},
		{"", "geo"},
		// A lock somebody hand-edited carries whatever they typed, and a name
		// that would not survive Clean is not one to build a path from.
		{"../../etc/passwd", "geo"},
		{"/absolute", "geo"},
		{".", "geo"},
	} {
		got := packFileName(name, project.LockAsset{Filename: testCase.recorded})
		if got != testCase.want {
			t.Errorf("packFileName(%q) = %q, want %q", testCase.recorded, got, testCase.want)
		}
	}
}

// TestPackDestinationRefusesAnythingOutsideTheDirectory covers the last check
// before the filesystem. Every input here is one the derivation would already
// have refused, which is the point: this is what a future change to the
// derivation would run into instead of a write outside the destination.
func TestPackDestinationRefusesAnythingOutsideTheDirectory(t *testing.T) {
	directory := t.TempDir()
	inside, err := packDestination(directory, "assets/app/geo/1/geo.bin")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(directory, "assets", "app", "geo", "1", "geo.bin"); inside != want {
		t.Fatalf("packDestination returned %q, want %q", inside, want)
	}
	for _, file := range []string{
		"../escape",
		"assets/../../escape",
		"assets/app/geo/1/../../../../../../etc/passwd",
		"/etc/passwd",
		"..",
	} {
		if got, err := packDestination(directory, file); err == nil {
			t.Errorf("packDestination accepted %q as %q", file, got)
		}
	}
}

// TestValidatePackIndexKeysOnTheDerivedPath is what makes an archive entry's
// name a lookup key rather than a destination. The map an entry is matched
// against is built from paths DAC computed, so a name that finds something has
// already been proven to be one of them.
func TestValidatePackIndexKeysOnTheDerivedPath(t *testing.T) {
	value := "sha256:" + strings.Repeat("a", 64)
	targets, err := validatePackIndex(packIndex{
		SchemaVersion: packSchemaVersion,
		Items: []PackItem{{
			Coordinate: "app/geo@1",
			SourceURL:  "https://example.com/geo.bin",
			File:       "assets/app/geo/1/geo.bin",
			Filename:   "geo.bin",
			Digest:     value,
			Size:       3,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	target, found := targets["assets/app/geo/1/geo.bin"]
	if !found {
		t.Fatalf("the index is keyed on %v", targets)
	}
	derived, err := packFilePath(coord.MustParse("app/geo@1"), "geo.bin")
	if err != nil {
		t.Fatal(err)
	}
	if target.path != derived {
		t.Fatalf("the target carries %q rather than the derived %q", target.path, derived)
	}
	if target.object.Digest != value || target.object.Size != 3 {
		t.Fatalf("unexpected object: %#v", target.object)
	}
}
