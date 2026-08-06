package application

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/fs/atomic"
)

// testUnpackStage builds a production-shaped stage so commit tests exercise the atomic file lifecycle.
func testUnpackStage(t *testing.T, destination, content string) *atomic.File {
	t.Helper()
	stage, err := createUnpackStage(destination)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stage.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stage.Discard() })
	return stage
}

func TestCommitUnpackUsesAtomicStagesForBothCommitModes(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "no replace", true: "force"}[force], func(t *testing.T) {
			directory := t.TempDir()
			destination := filepath.Join(directory, "asset.bin")
			if force {
				if err := os.WriteFile(destination, []byte("old asset"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			stage := testUnpackStage(t, destination, "new asset")
			if err := commitUnpack([]*unpackTarget{{destination: destination, staged: stage}}, force); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(destination)
			if err != nil || string(data) != "new asset" {
				t.Fatalf("destination = %q (%v), want the staged bytes", data, err)
			}
			info, err := os.Stat(destination)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o644 {
				t.Fatalf("destination mode = %v, want 0644", info.Mode())
			}
			staged, err := filepath.Glob(filepath.Join(directory, unpackTempPrefix+"*"))
			if err != nil || len(staged) != 0 {
				t.Fatalf("commit left temporary files: %v %v", staged, err)
			}
		})
	}
}

// TestCommitUnpackNamesADestinationThatAppeared covers the link that is the
// no-replace check. A file arriving after the check and before the commit is the
// one case the check cannot cover, and the answer to it is the same --force the
// check itself offers rather than a write failure somebody has to decode.
func TestCommitUnpackNamesADestinationThatAppeared(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "asset.bin")
	staged := testUnpackStage(t, destination, "new asset")
	if err := os.WriteFile(destination, []byte("somebody else"), 0o644); err != nil {
		t.Fatal(err)
	}
	targets := []*unpackTarget{{destination: destination, staged: staged}}

	value := fault.As(commitUnpack(targets, false))
	if value.Code != "unpack_destination_occupied" {
		t.Fatalf("code = %q, want unpack_destination_occupied", value.Code)
	}
	if !strings.Contains(value.Message, "--force") {
		t.Fatalf("message = %q, want the option that replaces it", value.Message)
	}
	if value.Details["files"] == nil {
		t.Fatalf("details = %v, want the file that was in the way", value.Details)
	}
	// The file that was already there is the one thing a refused commit must not have touched.
	content, err := os.ReadFile(destination)
	if err != nil || string(content) != "somebody else" {
		t.Fatalf("destination = %q (%v), want it left alone", content, err)
	}
	if _, err := os.Stat(staged.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the staged file survived a failed commit")
	}
}

func TestCommitUnpackRestoresReplacedFilesAfterFailure(t *testing.T) {
	directory := t.TempDir()
	firstPath := filepath.Join(directory, "first.bin")
	secondPath := filepath.Join(directory, "second.bin")
	for path, content := range map[string]string{
		firstPath:  "old first",
		secondPath: "old second",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	firstStage := testUnpackStage(t, firstPath, "new first")
	missingStage := testUnpackStage(t, secondPath, "new second")
	if err := os.Remove(missingStage.Path()); err != nil {
		t.Fatal(err)
	}
	targets := []*unpackTarget{
		{destination: firstPath, staged: firstStage},
		{destination: secondPath, staged: missingStage},
	}
	if err := commitUnpack(targets, true); err == nil {
		t.Fatal("commit succeeded with a missing staged file")
	}
	for path, want := range map[string]string{firstPath: "old first", secondPath: "old second"} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("%s has %q, want %q: %v", path, data, want, err)
		}
	}
	backups, err := filepath.Glob(filepath.Join(directory, unpackTempPrefix+"*"))
	if err != nil || len(backups) != 0 {
		t.Fatalf("rollback left temporary files: %v %v", backups, err)
	}
}
