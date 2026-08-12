package atomic

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type failFirstSyncFilesystem struct {
	pathFilesystem
	failed bool
}

// syncDir injects a failure only after the destination rename, then delegates
// subsequent calls so the rollback path can durably restore the old file.
func (filesystem *failFirstSyncFilesystem) syncDir(path string) error {
	if !filesystem.failed {
		filesystem.failed = true
		return errors.New("injected directory sync failure")
	}
	return filesystem.pathFilesystem.syncDir(path)
}

// A durability error happens after rename, so callers need the returned
// Commit to restore the previous destination as part of a larger transaction.
func TestReversibleCommitCanRollbackAPostRenameSyncFailure(t *testing.T) {
	directory := t.TempDir()
	destination := filepath.Join(directory, "object")
	if err := os.WriteFile(destination, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	filesystem := &failFirstSyncFilesystem{}
	file, err := createIn(filesystem, directory, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	file.destination = destination
	if _, err := file.Write([]byte("replacement")); err != nil {
		t.Fatal(err)
	}

	commit, err := file.CommitReversible()
	if err == nil || commit == nil {
		t.Fatalf("CommitReversible = (%v, %v), want a recoverable sync failure", commit, err)
	}
	if err := commit.Rollback(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "previous" {
		t.Fatalf("rollback restored %q, want previous", contents)
	}
}
