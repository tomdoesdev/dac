package cli

// Tests for narrowing an unpack, for the destination it writes into, and for
// the layout it leaves there. What is worth testing here is what does not reach
// the disk: an unpack given one asset has to leave the other two in the
// archive, one given a coordinate the archive does not carry has to leave all
// of them there, and none of them may leave the archive's own directories
// behind.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The assets every test here packs, the path each is served at, and the file
// name an unpack writes it under. The three names differ, so this project is
// one a flat unpack can write whole; the clash has its own project below.
var unpackAssets = []struct{ coordinate, path, file string }{
	{"app/geo@1.0.0", "/geo-1.bin", "geo-1.bin"},
	{"app/geo@2.0.0", "/geo-2.bin", "geo-2.bin"},
	{"tools/kit@3.0.0", "/kit.bin", "kit.bin"},
}

// TestUnpackWritesOnlyTheAssetsNamed covers the reason an unpack can be
// narrowed. A dacpack carries a whole project and what reaches for one asset
// usually wants one asset, so the alternative was materializing the project and
// deleting the rest of it.
func TestUnpackWritesOnlyTheAssetsNamed(t *testing.T) {
	archive := newPackedProject(t)
	directory := t.TempDir()

	result := runJSON(t, []string{"unpack", archive, "app/geo@1.0.0", "--dest", directory})
	assertSuccess(t, result, "unpack")
	data := result.value["data"].(map[string]any)
	if data["fileCount"] != float64(1) || data["itemCount"] != float64(3) {
		t.Fatalf("unexpected counts: %#v", data)
	}
	if written := unpackedFiles(t, directory); !slices.Equal(written, []string{unpackAssets[0].file}) {
		t.Fatalf("the unpack wrote %v, want only %s", written, unpackAssets[0].file)
	}
}

// TestUnpackNarrowedByAssetTakesEveryVersion covers the shorter spelling, which
// is the one info and pull already read.
func TestUnpackNarrowedByAssetTakesEveryVersion(t *testing.T) {
	archive := newPackedProject(t)
	directory := t.TempDir()

	assertSuccess(t, runJSON(t, []string{"unpack", archive, "app/geo", "--dest", directory}), "unpack")
	want := []string{unpackAssets[0].file, unpackAssets[1].file}
	if written := unpackedFiles(t, directory); !slices.Equal(written, want) {
		t.Fatalf("the unpack wrote %v, want both versions of app/geo", written)
	}
}

// TestUnpackWritesOverlappingSelectionsOnce covers a coordinate named twice,
// once whole and once by its asset. A selection is a filter over the archive
// rather than a list of files to write.
func TestUnpackWritesOverlappingSelectionsOnce(t *testing.T) {
	archive := newPackedProject(t)
	directory := t.TempDir()

	result := runJSON(t, []string{"unpack", archive, "app/geo", "app/geo@1.0.0", "--dest", directory})
	assertSuccess(t, result, "unpack")
	if data := result.value["data"].(map[string]any); data["fileCount"] != float64(2) {
		t.Fatalf("unexpected file count: %#v", data)
	}
	want := []string{unpackAssets[0].file, unpackAssets[1].file}
	if written := unpackedFiles(t, directory); !slices.Equal(written, want) {
		t.Fatalf("the unpack wrote %v, want one file per asset", written)
	}
}

// TestUnpackSaysWhenItTookOnlyPartOfTheArchive covers the summary, for the
// reason a narrowed pull has one: "Unpacked 1 file" is true of an archive with
// one asset and of a job that wanted one of three.
func TestUnpackSaysWhenItTookOnlyPartOfTheArchive(t *testing.T) {
	archive := newPackedProject(t)
	directory := t.TempDir()

	human := run(t, []string{"unpack", archive, "tools/kit@3.0.0", "--dest", directory})
	if human.status != ExitOK || !strings.HasPrefix(human.stdout, "Unpacked 1 file of 3 (") {
		t.Fatalf("unexpected narrowed unpack summary: %#v", human)
	}
	human = run(t, []string{"unpack", archive, "--dest", t.TempDir()})
	if human.status != ExitOK || !strings.HasPrefix(human.stdout, "Unpacked 3 files (") {
		t.Fatalf("unexpected whole unpack summary: %#v", human)
	}
}

// TestUnpackRefusesAnAssetTheDacpackDoesNotHave keeps a typo from being read as
// "write nothing and report success". The refusal names the archive rather than
// a project, because unpack never opened one.
func TestUnpackRefusesAnAssetTheDacpackDoesNotHave(t *testing.T) {
	archive := newPackedProject(t)
	directory := t.TempDir()

	result := runJSON(t, []string{"unpack", archive, "app/geo@9.9.9", "--dest", directory})
	assertError(t, result, "asset_unknown")
	message, _ := result.value["error"].(map[string]any)["message"].(string)
	if !strings.Contains(message, "dacpack") {
		t.Fatalf("the refusal does not name the dacpack: %q", message)
	}
	assertError(t, runJSON(t, []string{"unpack", archive, "nope/missing", "--dest", directory}), "asset_unknown")

	// A refusal writes nothing at all, including the asset that was there.
	if written := unpackedFiles(t, directory); len(written) != 0 {
		t.Fatalf("a refused unpack left %v behind", written)
	}
}

// TestUnpackWritesTheFilesThemselves is the layout. What comes out is the files
// a dacpack carries, in the destination, under the names their origins gave
// them -- not the archive's directories copied onto a disk, which is a
// container's way of keeping two assets apart and nobody else's problem.
func TestUnpackWritesTheFilesThemselves(t *testing.T) {
	archive := newPackedProject(t)
	directory := t.TempDir()

	assertSuccess(t, runJSON(t, []string{"unpack", archive, "--dest", directory}), "unpack")
	want := []string{"geo-1.bin", "geo-2.bin", "kit.bin"}
	if written := unpackedFiles(t, directory); !slices.Equal(written, want) {
		t.Fatalf("the unpack wrote %v, want %v", written, want)
	}
	// Flat means flat: nothing but files at the top of the destination.
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			t.Fatalf("the unpack left the directory %q behind", entry.Name())
		}
	}
}

// TestUnpackKeepsTheArchiveLayoutWithTree covers the option that puts the
// directories back, which is what an archive whose names collide has instead of
// a refusal.
func TestUnpackKeepsTheArchiveLayoutWithTree(t *testing.T) {
	archive := newPackedProject(t)
	directory := t.TempDir()

	assertSuccess(t, runJSON(t, []string{"unpack", archive, "--tree", "--dest", directory}), "unpack")
	want := []string{
		"assets/app/geo/1.0.0/geo-1.bin",
		"assets/app/geo/2.0.0/geo-2.bin",
		"assets/tools/kit/3.0.0/kit.bin",
	}
	if written := unpackedFiles(t, directory); !slices.Equal(written, want) {
		t.Fatalf("--tree wrote %v, want the archive's own paths", written)
	}
}

// TestUnpackRefusesTwoAssetsWithOneFileName covers what flat cannot do. Two
// versions of one asset usually share a file name, and both cannot be written
// to it -- so the unpack is refused rather than one of them being lost under a
// name the result says belongs to the other.
func TestUnpackRefusesTwoAssetsWithOneFileName(t *testing.T) {
	archive := newClashingProject(t)
	directory := t.TempDir()

	result := runJSON(t, []string{"unpack", archive, "--dest", directory})
	assertError(t, result, "unpack_name_collision")
	details := result.value["error"].(map[string]any)["details"].(map[string]any)
	if files := details["files"].([]any); len(files) != 1 || files[0] != "geo.bin" {
		t.Fatalf("the refusal does not name the file: %#v", details)
	}
	if assets := details["assets"].([]any); len(assets) != 2 ||
		assets[0] != "app/geo@1.0.0" || assets[1] != "app/geo@2.0.0" {
		t.Fatalf("the refusal does not name both assets: %#v", details)
	}
	// Force replaces what is on disk; it does not choose between two files the
	// command was told to write.
	assertError(t, runJSON(t, []string{"unpack", archive, "--force", "--dest", directory}), "unpack_name_collision")
	if written := unpackedFiles(t, directory); len(written) != 0 {
		t.Fatalf("a refused unpack left %v behind", written)
	}

	// Both ways through: name the version wanted, or take the archive's layout.
	assertSuccess(t, runJSON(t, []string{"unpack", archive, "app/geo@2.0.0", "--dest", directory}), "unpack")
	if written := unpackedFiles(t, directory); !slices.Equal(written, []string{"geo.bin"}) {
		t.Fatalf("the narrowed unpack wrote %v", written)
	}
	tree := t.TempDir()
	assertSuccess(t, runJSON(t, []string{"unpack", archive, "--tree", "--dest", tree}), "unpack")
	if written := unpackedFiles(t, tree); len(written) != 2 {
		t.Fatalf("--tree wrote %v, want both versions", written)
	}
}

// TestUnpackTakesItsDestinationFromTheOption covers --dest and the empty value
// it will not read as the working directory: an empty argument is a shell
// variable nobody set rather than a request to write here.
func TestUnpackTakesItsDestinationFromTheOption(t *testing.T) {
	archive := newPackedProject(t)
	directory := filepath.Join(t.TempDir(), "made", "by", "the", "unpack")

	result := runJSON(t, []string{"unpack", archive, "--dest", directory})
	assertSuccess(t, result, "unpack")
	if data := result.value["data"].(map[string]any); data["directory"] != directory {
		t.Fatalf("unexpected destination: %#v", data)
	}
	if written := unpackedFiles(t, directory); len(written) != 3 {
		t.Fatalf("the unpack wrote %v", written)
	}
	assertError(t, runJSON(t, []string{"unpack", archive, "--dest", "  "}), "invalid_arguments")
}

// TestUnpackSaysWhereTheDestinationArgumentWent covers the spelling a script
// written before --dest has. The second argument names an asset now, so
// answering "/opt/inputs is not a coordinate" would be true and useless.
func TestUnpackSaysWhereTheDestinationArgumentWent(t *testing.T) {
	archive := newPackedProject(t)

	for _, destination := range []string{filepath.Join(t.TempDir(), "out"), "./out", "out"} {
		result := runJSON(t, []string{"unpack", archive, destination})
		assertError(t, result, "invalid_arguments")
		message, _ := result.value["error"].(map[string]any)["message"].(string)
		if !strings.Contains(message, "--dest") {
			t.Fatalf("unpacking into %q does not mention --dest: %q", destination, message)
		}
	}
	// A coordinate with a typo in it is still a coordinate with a typo in it.
	result := runJSON(t, []string{"unpack", archive, "App/geo@1.0.0", "--dest", t.TempDir()})
	assertError(t, result, "invalid_arguments")
	message, _ := result.value["error"].(map[string]any)["message"].(string)
	if strings.Contains(message, "--dest") {
		t.Fatalf("a mistyped coordinate was answered with the destination option: %q", message)
	}
}

// newPackedProject locks a three-asset project and packs it, returning the
// archive. Unpack reads no project files, so nothing but the archive is needed
// from that point on.
func newPackedProject(t *testing.T) string {
	t.Helper()
	return packProject(t, unpackAssets)
}

// newClashingProject packs two versions of one asset served under one file
// name, which is what a URL that keeps the version in its path leaves: the
// lock records geo.bin for both, and flat they land on one path.
func newClashingProject(t *testing.T) string {
	t.Helper()
	return packProject(t, []struct{ coordinate, path, file string }{
		{"app/geo@1.0.0", "/v1/geo.bin", "geo.bin"},
		{"app/geo@2.0.0", "/v2/geo.bin", "geo.bin"},
	})
}

// packProject builds and packs a project from a list of assets, serving each
// one bytes that differ so a test can tell which file it is looking at.
func packProject(t *testing.T, assets []struct{ coordinate, path, file string }) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "bytes for "+request.URL.Path)
	}))
	t.Cleanup(server.Close)

	paths := newProject(t)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	for _, asset := range assets {
		assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", asset.coordinate, server.URL+asset.path)), "add")
	}
	archive := filepath.Join(t.TempDir(), "project.dacpack")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "pack", archive)), "pack")
	return archive
}

// unpackedFiles lists what an unpack left in a directory, as archive-relative
// slash-separated paths in a stable order.
func unpackedFiles(t *testing.T, directory string) []string {
	t.Helper()
	found := []string{}
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}
		found = append(found, filepath.ToSlash(relative))
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	slices.Sort(found)
	return found
}
