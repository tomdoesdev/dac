package catalog

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/digest"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	return New(filepath.Join(t.TempDir(), FileName))
}

// note applies one observation through the store, which is what every caller does.
func note(t *testing.T, store *Store, value, name string) {
	t.Helper()
	if _, err := store.Update(context.Background(), func(catalog Catalog) (Catalog, error) {
		return catalog.Note([]Entry{entry(value, name, "https://example.test/a", "a.bin")}, early), nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestResolvePathPutsTheCatalogInTheDataHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	path, err := ResolvePath("")
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	if path != filepath.Join(home, DirName, FileName) {
		t.Fatalf("path = %q, want it beside the other data DAC keeps", path)
	}
}

// TestLoadTreatsAMissingFileAsAnEmptyCatalog keeps a first run from failing before it has had
// the chance to record anything.
func TestLoadTreatsAMissingFileAsAnEmptyCatalog(t *testing.T) {
	catalog, err := testStore(t).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(catalog.Objects) != 0 || catalog.SchemaVersion != SchemaVersion {
		t.Fatalf("catalog = %#v, want an empty current-version catalog", catalog)
	}
}

// TestLoadKeepsACatalogItCannotRead ensures a malformed catalog is left for an
// operator to inspect rather than silently replaced with an empty record.
func TestLoadKeepsACatalogItCannotRead(t *testing.T) {
	store := testStore(t)
	damaged := []byte("{ this is not JSON")
	if err := os.WriteFile(store.Path, damaged, fileMode); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("Load accepted a catalog it could not parse")
	}
	data, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != string(damaged) {
		t.Fatalf("file is %q, want it left alone", data)
	}
}

func TestUpdateWritesAFileDACCanReadBack(t *testing.T) {
	store := testStore(t)
	note(t, store, first, "tools/rg@14.1.0")
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, held := loaded.Objects[first]; !held {
		t.Fatalf("loaded = %#v, want the record back", loaded)
	}
	data, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if text := string(data); text[len(text)-2:] != "}\n" {
		t.Fatalf("file is %q, want indented JSON ending in a newline", text)
	}
}

// TestUpdateWritesNothingWhenTheCatalogDidNotMove bounds what this costs. Every command that
// reads a project reaches the store, and most of them have nothing new to say.
func TestUpdateWritesNothingWhenTheCatalogDidNotMove(t *testing.T) {
	store := testStore(t)
	note(t, store, first, "tools/rg@14.1.0")
	stale := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(store.Path, stale, stale); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	note(t, store, first, "tools/rg@14.1.0")
	info, err := os.Stat(store.Path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.ModTime().Equal(stale) {
		t.Fatalf("the file was rewritten with nothing to change")
	}
}

// TestUpdateKeepsBothChangesWhenTwoRunsOverlap is why every mutation goes through the lock: two
// processes that each loaded, changed, and saved would keep only one of the two changes.
func TestUpdateKeepsBothChangesWhenTwoRunsOverlap(t *testing.T) {
	store := testStore(t)
	third := digest.Bytes([]byte("third object"))
	var group sync.WaitGroup
	for value, name := range map[string]string{second: "tools/jq@1.7", third: "tools/rg@14.1.0"} {
		group.Go(func() {
			// Failures are reported rather than fatal, because only the test goroutine may stop
			// the test.
			if _, err := store.Update(context.Background(), func(catalog Catalog) (Catalog, error) {
				return catalog.Note([]Entry{entry(value, name, "https://example.test/a", "a.bin")}, early), nil
			}); err != nil {
				t.Errorf("Update: %v", err)
			}
		})
	}
	group.Wait()
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Objects) != 2 {
		t.Fatalf("catalog = %#v, want both records", loaded)
	}
}
