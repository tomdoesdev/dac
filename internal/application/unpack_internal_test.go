package application

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tomdoesdev/dac/internal/fault"
)

// TestCommitUnpackNamesADestinationThatAppeared covers the link that is the
// no-replace check. A file arriving after the check and before the commit is the
// one case the check cannot cover, and the answer to it is the same --force the
// check itself offers rather than a write failure somebody has to decode.
func TestCommitUnpackNamesADestinationThatAppeared(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "asset.bin")
	staged := filepath.Join(directory, "asset.stage")
	for path, content := range map[string]string{destination: "somebody else", staged: "new asset"} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
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
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the staged file survived a failed commit")
	}
}

func TestCommitUnpackRestoresReplacedFilesAfterFailure(t *testing.T) {
	directory := t.TempDir()
	firstPath := filepath.Join(directory, "first.bin")
	secondPath := filepath.Join(directory, "second.bin")
	firstStage := filepath.Join(directory, "first.stage")
	missingStage := filepath.Join(directory, "missing.stage")
	for path, content := range map[string]string{
		firstPath:  "old first",
		secondPath: "old second",
		firstStage: "new first",
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
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
	backups, err := filepath.Glob(filepath.Join(directory, ".dac-unpack-*"))
	if err != nil || len(backups) != 0 {
		t.Fatalf("rollback left temporary files: %v %v", backups, err)
	}
}
