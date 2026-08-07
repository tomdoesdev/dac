package atomic_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tomdoesdev/kit/fs/atomic"
)

// This is the test the package exists for. Writers replace one file while a
// reader reads it, and every read has to be a whole file that one writer wrote.
func TestConcurrentWritesNeverExposePartialContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object")
	const writers = 8
	const size = 32 * 1024

	stop := make(chan struct{})
	var reader sync.WaitGroup
	reader.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
			}
			data, err := os.ReadFile(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Errorf("ReadFile returned %v", err)
				return
			}
			if len(data) != size {
				t.Errorf("a reader saw %d bytes, want %d", len(data), size)
				return
			}
			if bytes.Count(data, data[:1]) != size {
				t.Errorf("a reader saw a file of mixed contents, starting %q", data[0])
				return
			}
		}
	})

	var group sync.WaitGroup
	for index := range writers {
		group.Go(func() {
			payload := bytes.Repeat([]byte{byte('a' + index)}, size)
			for range 10 {
				if err := atomic.WriteFile(path, payload, 0o644); err != nil {
					t.Errorf("WriteFile returned %v", err)
					return
				}
			}
		})
	}
	group.Wait()
	close(stop)
	reader.Wait()
}

func TestWriteFileCreatesTheFileAndItsParents(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "nested", "deep", "object")
	// A umask of 022 would report 0o644 here if the mode were being narrowed.
	if err := atomic.WriteFile(path, []byte("contents"), 0o666); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "contents", 0o666)
	assertOnly(t, filepath.Dir(path), "object")
}

// A rename replaces the file, so the new mode is the one Create was given
// rather than the one the old file had.
func TestWriteFileReplacesTheContentsAndTheMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object")
	if err := atomic.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := atomic.WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "new", 0o644)
	assertOnly(t, filepath.Dir(path), "object")
}

func TestWriteFileLockedRemovesItsSidecar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object")
	if err := atomic.WriteFileLocked(path, []byte("contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "contents", 0o600)
	assertOnly(t, filepath.Dir(path), "object")
}

func TestCommitPutsTheFileInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object")
	file, err := atomic.Create(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Discard() }()
	if _, err := file.Write([]byte("contents")); err != nil {
		t.Fatal(err)
	}
	// Nothing is in place until the commit.
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the destination exists before the commit: %v", err)
	}
	if err := file.Commit(); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "contents", 0o600)
	assertOnly(t, filepath.Dir(path), "object")
}

// The deferred discard in every other test depends on this.
func TestDiscardAfterACommitDoesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object")
	file, err := atomic.Create(path, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("contents")); err != nil {
		t.Fatal(err)
	}
	if err := file.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := file.Discard(); err != nil {
		t.Fatalf("Discard after a commit returned %v", err)
	}
	if err := file.Discard(); err != nil {
		t.Fatalf("a second Discard returned %v", err)
	}
	if err := file.Commit(); err != nil {
		t.Fatalf("a second Commit returned %v", err)
	}
	assertFile(t, path, "contents", 0o644)
}

func TestDiscardLeavesTheDestinationAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object")
	if err := atomic.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := atomic.Create(path, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("abandoned")); err != nil {
		t.Fatal(err)
	}
	if err := file.Discard(); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "old", 0o644)
	assertOnly(t, filepath.Dir(path), "object")
}

// A commit that fails leaves the temporary file, because the destination is
// still the caller's to write. The deferred discard is what removes it.
func TestAFailedCommitLeavesTheTemporaryFileToDiscard(t *testing.T) {
	directory := t.TempDir()
	blocker := filepath.Join(directory, "blocker")
	if err := atomic.WriteFile(blocker, []byte("a file, not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := atomic.CreateIn(directory, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("contents")); err != nil {
		t.Fatal(err)
	}
	if err := file.CommitAs(filepath.Join(blocker, "object")); err == nil {
		t.Fatal("a commit under a regular file succeeded")
	}
	if _, err := os.Stat(file.Path()); err != nil {
		t.Fatalf("the failed commit removed the temporary file: %v", err)
	}
	if err := file.Discard(); err != nil {
		t.Fatal(err)
	}
	assertOnly(t, directory, "blocker")
}

func TestCommitAsWritesIntoAnotherDirectory(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, "tmp")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := atomic.CreateIn(staging, 0o444)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Discard() }()
	if _, err := file.Write([]byte("contents")); err != nil {
		t.Fatal(err)
	}
	// The destination decides itself only now, and its parents do not exist.
	path := filepath.Join(root, "ab", "cdef", "object")
	if err := file.CommitAs(path); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "contents", 0o444)
	assertOnly(t, staging)
}

func TestCommitNoReplaceInstallsAnAbsentDestination(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "object")
	file, err := atomic.Create(path, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Discard() }()
	if _, err := file.Write([]byte("contents")); err != nil {
		t.Fatal(err)
	}
	commit, err := file.CommitNoReplace()
	if err != nil {
		t.Fatal(err)
	}
	if err := commit.Complete(); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "contents", 0o640)
	assertOnly(t, directory, "object")
}

func TestCommitNoReplaceLeavesAnExistingDestinationAlone(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "object")
	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := atomic.Create(path, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("replacement")); err != nil {
		t.Fatal(err)
	}
	if commit, err := file.CommitNoReplace(); !errors.Is(err, fs.ErrExist) || commit != nil {
		t.Fatalf("CommitNoReplace = (%v, %v), want nil and fs.ErrExist", commit, err)
	}
	if err := file.Discard(); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "existing", 0o600)
	assertOnly(t, directory, "object")
}

func TestNoReplaceCommitCanBeRolledBack(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "object")
	file, err := atomic.Create(path, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("contents")); err != nil {
		t.Fatal(err)
	}
	commit, err := file.CommitNoReplace()
	if err != nil {
		t.Fatal(err)
	}
	if err := commit.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("destination survived rollback: %v", err)
	}
	assertOnly(t, directory)
}

func TestReversibleCommitRestoresThePreviousDestination(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "object")
	if err := os.WriteFile(path, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := atomic.Create(path, 0o644, atomic.WithTempPrefix(".change-"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("replacement")); err != nil {
		t.Fatal(err)
	}
	commit, err := file.CommitReversible()
	if err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "replacement", 0o644)
	if err := commit.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "previous", 0o600)
	assertOnly(t, directory, "object")
}

func TestCompletingAReversibleCommitKeepsTheReplacement(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "object")
	if err := os.WriteFile(path, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := atomic.Create(path, 0o644, atomic.WithTempPrefix(".change-"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("replacement")); err != nil {
		t.Fatal(err)
	}
	commit, err := file.CommitReversible()
	if err != nil {
		t.Fatal(err)
	}
	if err := commit.Complete(); err != nil {
		t.Fatal(err)
	}
	if err := commit.Complete(); err != nil {
		t.Fatalf("second Complete returned %v", err)
	}
	if err := commit.Rollback(); err != nil {
		t.Fatalf("Rollback after Complete returned %v", err)
	}
	assertFile(t, path, "replacement", 0o644)
	assertOnly(t, directory, "object")
}

func TestCommitWithoutADestinationFails(t *testing.T) {
	directory := t.TempDir()
	file, err := atomic.CreateIn(directory, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	err = file.Commit()
	if err == nil {
		t.Fatal("a commit with no destination succeeded")
	}
	// The message has to say what to call instead, because this is a mistake
	// in the calling code rather than a state to recover from.
	if !strings.Contains(err.Error(), "CommitAs") {
		t.Fatalf("the error does not name CommitAs: %v", err)
	}
	if err := file.Discard(); err != nil {
		t.Fatal(err)
	}
	assertOnly(t, directory)
}

func TestWithoutMkdirCreatesNothing(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "nested", "object")
	err := atomic.WriteFile(path, []byte("contents"), 0o644, atomic.WithoutMkdir())
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("WriteFile returned %v, want a missing directory", err)
	}
	assertOnly(t, directory)
}

func TestTheTemporaryFileCarriesItsPrefix(t *testing.T) {
	directory := t.TempDir()
	file, err := atomic.Create(filepath.Join(directory, "object"), 0o644, atomic.WithTempPrefix(".dac-"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Discard() }()
	name := filepath.Base(file.Path())
	if !strings.HasPrefix(name, ".dac-") {
		t.Fatalf("the temporary file is named %q", name)
	}
	// A caller that sweeps leftovers matches what is on disk, not what Path
	// reports, so check that they agree.
	assertOnly(t, directory, name)
}

func TestWithoutSyncStillCommits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "object")
	if err := atomic.WriteFile(path, []byte("contents"), 0o644, atomic.WithoutSync()); err != nil {
		t.Fatal(err)
	}
	assertFile(t, path, "contents", 0o644)
	assertOnly(t, filepath.Dir(path), "object")
}

// A rename replaces a name rather than following it. This pins the behaviour
// that surprises people: a symbolic link at the destination is what goes.
func TestCommitReplacesASymbolicLinkRatherThanItsTarget(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	link := filepath.Join(directory, "link")
	if err := atomic.WriteFile(target, []byte("target"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := atomic.WriteFile(link, []byte("through the link"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		t.Fatal("the destination is still a symbolic link")
	}
	assertFile(t, target, "target", 0o644)
}

func assertFile(t *testing.T, path, contents string, perm fs.FileMode) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != contents {
		t.Fatalf("%s holds %q, want %q", path, data, contents)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != perm {
		t.Fatalf("%s has mode %v, want %v", path, info.Mode().Perm(), perm)
	}
}

// assertOnly reports every entry in the directory that the test did not name.
// A temporary file left behind shows up here and nowhere else.
func assertOnly(t *testing.T, directory string, names ...string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		found = append(found, entry.Name())
	}
	want := strings.Join(names, ",")
	if got := strings.Join(found, ","); got != want {
		t.Fatalf("%s holds [%s], want [%s]", directory, got, want)
	}
}
