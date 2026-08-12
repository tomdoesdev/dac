package project

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// openTestDownloads creates a project whose managed directory holds names.
func openTestDownloads(t *testing.T, names ...string) *Downloads {
	t.Helper()
	paths := Paths{Root: t.TempDir()}
	downloads, err := paths.OpenDownloads()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = downloads.Close() })
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(paths.Root, downloadsDir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return downloads
}

func present(t *testing.T, downloads *Downloads, name string) bool {
	t.Helper()
	_, err := downloads.Root().Lstat(name)
	return err == nil
}

// TestPruneRemovesOnlyPreviouslyAcceptedFiles pins the rule that made this
// worth owning here: prune is allowed to delete a file the previous accepted
// state claimed and the next one does not, and nothing else. An entry dac
// never tracked is reported by status, not silently removed.
func TestPruneRemovesOnlyPreviouslyAcceptedFiles(t *testing.T) {
	downloads := openTestDownloads(t, "obsolete.bin", "retained.bin", "untracked.bin")

	warnings := downloads.Prune([]string{"obsolete.bin", "retained.bin"}, []string{"retained.bin"})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %q, want none", warnings)
	}
	if present(t, downloads, "obsolete.bin") {
		t.Error("obsolete download survived prune")
	}
	if !present(t, downloads, "retained.bin") {
		t.Error("retained download was removed")
	}
	if !present(t, downloads, "untracked.bin") {
		t.Error("prune removed a file dac never tracked")
	}
}

// TestPruneReportsWhatItCannotRemove keeps an untidy directory from failing a
// command that has already installed correct state.
func TestPruneReportsWhatItCannotRemove(t *testing.T) {
	downloads := openTestDownloads(t)
	if err := downloads.Root().Mkdir("directory.bin", 0o755); err != nil {
		t.Fatal(err)
	}

	warnings := downloads.Prune([]string{"directory.bin", "never-existed.bin"}, nil)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %q, want exactly one", warnings)
	}
	if !present(t, downloads, "directory.bin") {
		t.Error("prune removed a directory it warned about")
	}
}

// TestPruneToleratesRepeatedNames guards the case where two lock entries
// resolved to the same destination before the manifest was corrected.
func TestPruneToleratesRepeatedNames(t *testing.T) {
	downloads := openTestDownloads(t, "shared.bin")
	if warnings := downloads.Prune([]string{"shared.bin", "shared.bin"}, nil); len(warnings) != 0 {
		t.Fatalf("warnings = %q, want none", warnings)
	}
	if present(t, downloads, "shared.bin") {
		t.Error("shared download survived prune")
	}
}

// TestUnreferencedListsOnlyUnclaimedEntries backs status's orphan report.
func TestUnreferencedListsOnlyUnclaimedEntries(t *testing.T) {
	downloads := openTestDownloads(t, "claimed.bin", "stray.bin", "another.bin")

	got, err := downloads.Unreferenced([]string{"claimed.bin"})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"another.bin", "stray.bin"}; !slices.Equal(got, want) {
		t.Fatalf("Unreferenced = %q, want %q", got, want)
	}
}

// TestOpenDownloadsOptionalDoesNotCreate keeps status free of side effects on a
// project that has never downloaded anything.
func TestOpenDownloadsOptionalDoesNotCreate(t *testing.T) {
	paths := Paths{Root: t.TempDir()}
	downloads, exists, err := paths.OpenDownloadsOptional()
	if err != nil {
		t.Fatal(err)
	}
	if exists || downloads != nil {
		t.Fatalf("OpenDownloadsOptional reported exists=%v on an empty project", exists)
	}
	if _, err := os.Stat(filepath.Join(paths.Root, downloadsDir)); !os.IsNotExist(err) {
		t.Fatalf("OpenDownloadsOptional created the managed directory: %v", err)
	}
}
