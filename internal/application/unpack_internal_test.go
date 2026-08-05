package application

import (
	"os"
	"path/filepath"
	"testing"
)

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
