package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/tomdoesdev/dac/internal/catalog"
	"github.com/tomdoesdev/dac/internal/coord"
)

// catalogHome points this test at a catalog of its own and returns the file it will write.
// Without it every test in this package would record into one shared file.
func catalogHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_DATA_HOME", home)
	return filepath.Join(home, catalog.DirName, catalog.FileName)
}

// servedProject sets up one asset served over loopback and the project that names it.
type servedProject struct {
	base         []string
	url          string
	manifestPath string
	lockPath     string
}

func serveProject(t *testing.T, content []byte) servedProject {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Disposition", `attachment; filename="geo.bin"`)
		_, _ = writer.Write(content)
	}))
	t.Cleanup(server.Close)
	directory := t.TempDir()
	served := servedProject{
		url:          server.URL,
		manifestPath: filepath.Join(directory, "dac.json"),
		lockPath:     filepath.Join(directory, "dac-lock.json"),
	}
	served.base = []string{
		"--manifest", served.manifestPath,
		"--lock", served.lockPath,
		"--cache-dir", filepath.Join(directory, "cache"),
	}
	return served
}

// add creates the project and lets pull resolve its one asset.
func (served servedProject) add(t *testing.T) {
	t.Helper()
	assertSuccess(t, runJSON(t, appendArgs(served.base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(served.base, "add", "app/geo@2026.08", served.url, "--no-progress")), "add")
	settleCLIProject(t, served.base)
}

func loadCatalog(t *testing.T, path string) catalog.Catalog {
	t.Helper()
	loaded, err := catalog.New(path).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return loaded
}

// TestCacheListNamesObjectsAfterTheProjectIsDeleted is the whole point of the catalog. Deleting
// the project files used to take with them the only record of what the cache was holding, and
// the listing that would have said so refused to run at all without a manifest to read.
func TestCacheListNamesObjectsAfterTheProjectIsDeleted(t *testing.T) {
	catalogHome(t)
	served := serveProject(t, []byte("mini dac asset"))
	served.add(t)

	if err := os.Remove(served.manifestPath); err != nil {
		t.Fatalf("Remove manifest: %v", err)
	}
	if err := os.Remove(served.lockPath); err != nil {
		t.Fatalf("Remove lock: %v", err)
	}

	result := runJSON(t, appendArgs(served.base, "cache", "list", "--all"))
	assertSuccess(t, result, "cache.list")
	data := result.value["data"].(map[string]any)
	if data["objectCount"] != float64(1) || data["unknownCount"] != float64(0) {
		t.Fatalf("unexpected listing: %#v", data)
	}
	object := data["objects"].([]any)[0].(map[string]any)
	if object["filename"] != "geo.bin" || object["sourceUrl"] != served.url {
		t.Fatalf("the listing did not name the object: %#v", object)
	}
	if known := object["knownAs"].([]any); len(known) != 1 || known[0] != "app/geo@2026.08" {
		t.Fatalf("the listing did not recall the coordinate: %#v", object)
	}
	// The project is gone, so nothing claims the object now. That is a different fact from what
	// it once was, and the listing keeps them apart.
	if _, claimed := object["coordinates"]; claimed {
		t.Fatalf("the listing claimed a coordinate no project names: %#v", object)
	}
}

// TestCacheListNeedsAProjectToListOnlyItsOwn keeps the change above to the question it answers.
// Listing this project's objects still has to know what this project is.
func TestCacheListNeedsAProjectToListOnlyItsOwn(t *testing.T) {
	catalogHome(t)
	served := serveProject(t, []byte("mini dac asset"))
	assertError(t, runJSON(t, appendArgs(served.base, "cache", "list")), "manifest_missing")
}

// TestReadingAProjectRecordsWhatItsObjectsAre covers the commands that download nothing. They
// are the ones that keep a record fresh between pulls.
func TestReadingAProjectRecordsWhatItsObjectsAre(t *testing.T) {
	catalogPath := catalogHome(t)
	served := serveProject(t, []byte("mini dac asset"))
	served.add(t)

	before := loadCatalog(t, catalogPath)
	if len(before.Objects) != 1 {
		t.Fatalf("catalog = %#v, want the added object", before)
	}
	if err := os.Remove(catalogPath); err != nil {
		t.Fatalf("Remove catalog: %v", err)
	}

	// Path reaches the cache without reaching the network, and rebuilds the record from the lock.
	assertSuccess(t, runJSON(t, appendArgs(served.base, "path", "app/geo@2026.08")), "path")
	after := loadCatalog(t, catalogPath)
	if len(after.Objects) != 1 {
		t.Fatalf("catalog = %#v, want the object recorded again", after)
	}
	for _, object := range after.Objects {
		source, recorded := object.Sources[coord.MustParse("app/geo@2026.08")]
		if !recorded || source.Filename != "geo.bin" {
			t.Fatalf("object = %#v, want the coordinate and file name", object)
		}
	}
}

// TestCollectionForgetsTheObjectsItRemoved is what bounds the file: a record describes what the
// cache holds, so it goes when the object does.
func TestCollectionForgetsTheObjectsItRemoved(t *testing.T) {
	catalogPath := catalogHome(t)
	served := serveProject(t, []byte("mini dac asset"))
	served.add(t)
	if len(loadCatalog(t, catalogPath).Objects) != 1 {
		t.Fatal("the added object was not recorded")
	}

	// A dry run removes nothing and so forgets nothing.
	assertSuccess(t, runJSON(t, appendArgs(served.base, "cache", "clear", "--dry-run")), "cache.clear")
	if len(loadCatalog(t, catalogPath).Objects) != 1 {
		t.Fatal("a dry run forgot an object it did not remove")
	}

	assertSuccess(t, runJSON(t, appendArgs(served.base, "cache", "clear")), "cache.clear")
	if remaining := loadCatalog(t, catalogPath).Objects; len(remaining) != 0 {
		t.Fatalf("catalog = %#v, want an emptied cache to leave an empty catalog", remaining)
	}
}
