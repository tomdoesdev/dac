package application_test

// Tests for the materialized archive. Its paths carry names an origin chose
// rather than digests DAC computed, so most of what is worth testing here is
// about those names: that they survive the round trip, that two assets cannot
// land on one path, and that a name arriving from outside cannot become a path
// DAC follows.

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/cache"
	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
)

// packedProject writes a project whose assets are described exactly as given,
// so a test can control the recorded file names that decide the layout.
func packedProject(t *testing.T, assets map[coord.Coordinate]project.LockAsset) (string, string) {
	t.Helper()
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	sources := make(map[coord.Coordinate]project.Asset, len(assets))
	for name, locked := range assets {
		sources[name] = project.Asset{URL: locked.URL}
	}
	manifest := project.Manifest{SchemaVersion: project.ManifestVersion, Assets: sources}
	lock, err := project.NewLock(manifest, assets)
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WritePair(manifestPath, lockPath, manifest, lock); err != nil {
		t.Fatal(err)
	}
	return manifestPath, lockPath
}

// packEntries reads an archive into its index and its file contents by path.
func packEntries(t *testing.T, path string) (map[string]any, map[string][]byte, []string) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	header, err := reader.Next()
	if err != nil {
		t.Fatal(err)
	}
	// The index leads, so a reader knows what to expect before the first file.
	if header.Name != "index.json" {
		t.Fatalf("the first entry is %q", header.Name)
	}
	var index map[string]any
	if err := json.NewDecoder(reader).Decode(&index); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	order := []string{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = data
		order = append(order, header.Name)
	}
	return index, files, order
}

// TestPackMaterializesEachAssetUnderItsOwnName covers what the archive is for.
// Extracting it with tar has to leave a directory of real files with real
// extensions, because that is what a machine which does not run DAC can use,
// and a tree of sha256 hashes is not.
func TestPackMaterializesEachAssetUnderItsOwnName(t *testing.T) {
	eleven, seventeen := []byte("jdk eleven bytes"), []byte("jdk seventeen bytes")
	manifestPath, lockPath := packedProject(t, map[coord.Coordinate]project.LockAsset{
		coord.MustParse("java/sdk@11"): {
			URL: "https://example.com/jdk-11.tar.gz", Digest: digest.Bytes(eleven),
			Size: int64(len(eleven)), Filename: "jdk-11.tar.gz",
		},
		coord.MustParse("java/sdk@17"): {
			URL: "https://example.com/jdk-17.tar.gz", Digest: digest.Bytes(seventeen),
			Size: int64(len(seventeen)), Filename: "jdk-17.tar.gz",
		},
	})
	store := cache.New(t.TempDir())
	warmStore(t, store, eleven)
	warmStore(t, store, seventeen)
	service := application.New(manifestPath, lockPath, store, nil, nil)

	archive := filepath.Join(t.TempDir(), "project.dacpack")
	result, err := service.Pack(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	if result.Pack != archive || result.AssetCount != 2 {
		t.Fatalf("unexpected pack result: %#v", result)
	}
	if want := int64(len(eleven) + len(seventeen)); result.ByteCount != want {
		t.Fatalf("pack reported %d bytes, want %d", result.ByteCount, want)
	}

	index, files, _ := packEntries(t, archive)
	if index["schemaVersion"] != float64(1) {
		t.Fatalf("unexpected schema version: %#v", index["schemaVersion"])
	}
	// Two versions of one asset share a file name, so the coordinate has to
	// supply the directories or the second would silently overwrite the first.
	expected := map[string][]byte{
		"assets/java/sdk/11/jdk-11.tar.gz": eleven,
		"assets/java/sdk/17/jdk-17.tar.gz": seventeen,
	}
	if len(files) != len(expected) {
		t.Fatalf("the archive holds %d files: %#v", len(files), files)
	}
	for path, content := range expected {
		if !bytes.Equal(files[path], content) {
			t.Fatalf("%s holds %q, want %q", path, files[path], content)
		}
	}

	items := index["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("the index lists %d items", len(items))
	}
	first := items[0].(map[string]any)
	if first["coordinate"] != "java/sdk@11" || first["file"] != "assets/java/sdk/11/jdk-11.tar.gz" ||
		first["filename"] != "jdk-11.tar.gz" || first["digest"] != digest.Bytes(eleven) ||
		first["sourceUrl"] != "https://example.com/jdk-11.tar.gz" || first["size"] != float64(len(eleven)) {
		t.Fatalf("unexpected index item: %#v", first)
	}
}

// TestPackFallsBackToTheCoordinateName covers an asset whose lock carries no
// name -- a URL that spelled none, a header DAC refused, or a lock written
// before names were recorded. The archive still needs a path for it, and the
// coordinate always has one that coord has already checked.
func TestPackFallsBackToTheCoordinateName(t *testing.T) {
	content := []byte("nameless bytes")
	manifestPath, lockPath := packedProject(t, map[coord.Coordinate]project.LockAsset{
		coord.MustParse("app/geo@1"): {
			URL: "https://example.com/download", Digest: digest.Bytes(content), Size: int64(len(content)),
		},
	})
	store := cache.New(t.TempDir())
	warmStore(t, store, content)

	archive := filepath.Join(t.TempDir(), "project.dacpack")
	if _, err := application.New(manifestPath, lockPath, store, nil, nil).Pack(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	_, files, _ := packEntries(t, archive)
	if !bytes.Equal(files["assets/app/geo/1/geo"], content) {
		t.Fatalf("the archive holds %#v", files)
	}
}

// TestPackWritesSharedBytesOncePerAsset is the cost a materialized archive
// pays. Two coordinates that resolved to the same object are one object in the
// cache and two files here, because each is materialized under its own name and
// a file cannot be in two places.
func TestPackWritesSharedBytesOncePerAsset(t *testing.T) {
	content := []byte("one set of bytes")
	manifestPath, lockPath := packedProject(t, map[coord.Coordinate]project.LockAsset{
		coord.MustParse("backend/geo@1"): {
			URL: "https://example.com/geo.bin", Digest: digest.Bytes(content),
			Size: int64(len(content)), Filename: "geo.bin",
		},
		coord.MustParse("frontend/geo@1"): {
			URL: "https://example.com/geo.bin", Digest: digest.Bytes(content),
			Size: int64(len(content)), Filename: "geo.bin",
		},
	})
	store := cache.New(t.TempDir())
	warmStore(t, store, content)

	archive := filepath.Join(t.TempDir(), "project.dacpack")
	if _, err := application.New(manifestPath, lockPath, store, nil, nil).Pack(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	_, files, order := packEntries(t, archive)
	if len(order) != 2 {
		t.Fatalf("the archive holds %d files, want one per asset: %v", len(order), order)
	}
	for _, path := range []string{"assets/backend/geo/1/geo.bin", "assets/frontend/geo/1/geo.bin"} {
		if !bytes.Equal(files[path], content) {
			t.Fatalf("%s holds %q", path, files[path])
		}
	}
}

// TestUnpackMaterializesWhatPackWrote is the round trip. Unpack writes files
// and never touches the cache, so what it leaves behind is a directory of real
// files something that has never heard of DAC can read -- the files themselves,
// in the destination, rather than the archive's layout copied onto a disk.
func TestUnpackMaterializesWhatPackWrote(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	source := cache.New(t.TempDir())
	warmStore(t, source, content)
	archive := filepath.Join(t.TempDir(), "project.dacpack")
	if _, err := application.New(manifestPath, lockPath, source, nil, nil).Pack(context.Background(), archive); err != nil {
		t.Fatal(err)
	}

	directory := t.TempDir()
	// No store and no project paths: unpack needs neither, which is what lets it
	// run on a machine that has no cache directory at all.
	result, err := application.New("", "", nil, nil, nil).Unpack(context.Background(), application.UnpackOptions{
		Pack: archive, Directory: directory,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Pack != archive || result.Directory != directory ||
		result.ItemCount != 1 || result.FileCount != 1 || result.ByteCount != int64(len(content)) {
		t.Fatalf("unexpected unpack result: %#v", result)
	}
	target := filepath.Join(directory, "asset")
	written, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(written, content) {
		t.Fatalf("the unpack wrote %q to %s: %v", written, target, err)
	}
	if len(result.Files) != 1 || result.Files[0].Path != target ||
		result.Files[0].Coordinate != "test/asset@1" || result.Files[0].Digest != digest.Bytes(content) {
		t.Fatalf("unexpected file report: %#v", result.Files)
	}
	// Nothing but the file: an archive's directories are how it keeps two assets
	// apart inside itself, and they have no business on the far side.
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "asset" || entries[0].IsDir() {
		t.Fatalf("the unpack left %#v", entries)
	}
}

// TestUnpackKeepsTheArchiveLayoutWithTree covers the other layout, which is
// what an archive holding two assets of one name has instead of a refusal.
func TestUnpackKeepsTheArchiveLayoutWithTree(t *testing.T) {
	archive, contents := packedArchive(t)
	directory := t.TempDir()

	result, err := application.New("", "", nil, nil, nil).Unpack(context.Background(), application.UnpackOptions{
		Pack: archive, Directory: directory, Tree: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FileCount != 2 {
		t.Fatalf("--tree wrote %d files", result.FileCount)
	}
	// The paths are the archive's own, which is what makes this the same tree
	// tar -xf would leave.
	for name, content := range contents {
		path := filepath.Join(directory, "assets/java/sdk", packedVersions[name], name)
		if written, readErr := os.ReadFile(path); readErr != nil || !bytes.Equal(written, content) {
			t.Fatalf("%s holds %q: %v", path, written, readErr)
		}
	}
}

// packedVersions maps each file packedArchive materializes to the version whose
// directory carries it inside the archive, for a test asserting the tree.
var packedVersions = map[string]string{"jdk-11.tar.gz": "11", "jdk-17.tar.gz": "17"}

// packedArchive writes a two-version project and packs it, returning the
// archive and the bytes behind each file name it carries.
//
// The two versions carry different file names, which is what makes this archive
// one a flat unpack can write whole. The clash is its own fixture below.
func packedArchive(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	eleven, seventeen := []byte("jdk eleven bytes"), []byte("jdk seventeen bytes")
	manifestPath, lockPath := packedProject(t, map[coord.Coordinate]project.LockAsset{
		coord.MustParse("java/sdk@11"): {
			URL: "https://example.com/jdk-11.tar.gz", Digest: digest.Bytes(eleven),
			Size: int64(len(eleven)), Filename: "jdk-11.tar.gz",
		},
		coord.MustParse("java/sdk@17"): {
			URL: "https://example.com/jdk-17.tar.gz", Digest: digest.Bytes(seventeen),
			Size: int64(len(seventeen)), Filename: "jdk-17.tar.gz",
		},
	})
	store := cache.New(t.TempDir())
	warmStore(t, store, eleven)
	warmStore(t, store, seventeen)
	archive := filepath.Join(t.TempDir(), "project.dacpack")
	if _, err := application.New(manifestPath, lockPath, store, nil, nil).Pack(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	return archive, map[string][]byte{
		"jdk-11.tar.gz": eleven,
		"jdk-17.tar.gz": seventeen,
	}
}

// clashingArchive packs two assets that carry one file name, which is the
// ordinary case: a version is rarely in the file name a URL spells, so two
// versions of one asset usually share theirs.
func clashingArchive(t *testing.T) string {
	t.Helper()
	first, second := []byte("geo one bytes"), []byte("geo two bytes")
	manifestPath, lockPath := packedProject(t, map[coord.Coordinate]project.LockAsset{
		coord.MustParse("app/geo@1"): {
			URL: "https://example.com/v1/geo.bin", Digest: digest.Bytes(first),
			Size: int64(len(first)), Filename: "geo.bin",
		},
		coord.MustParse("app/geo@2"): {
			URL: "https://example.com/v2/geo.bin", Digest: digest.Bytes(second),
			Size: int64(len(second)), Filename: "geo.bin",
		},
	})
	store := cache.New(t.TempDir())
	warmStore(t, store, first)
	warmStore(t, store, second)
	archive := filepath.Join(t.TempDir(), "clash.dacpack")
	if _, err := application.New(manifestPath, lockPath, store, nil, nil).Pack(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	return archive
}

// TestUnpackRefusesTwoAssetsWithOneFileName is what flat costs and what it
// refuses to pay. Both files cannot be at one path, so writing either would
// leave bytes under a name that says they belong to the other -- a file that
// looks like what was asked for and is not.
func TestUnpackRefusesTwoAssetsWithOneFileName(t *testing.T) {
	archive := clashingArchive(t)
	directory := t.TempDir()
	service := application.New("", "", nil, nil, nil)

	err := unpackClash(t, service, archive, directory, false)
	value := fault.As(err)
	if value.Code != "unpack_name_collision" {
		t.Fatalf("expected unpack_name_collision, got %q (%v)", value.Code, err)
	}
	// The refusal names the file and both assets, because the answer is to pick
	// one of them and it has to say which there are.
	files, _ := value.Details["files"].([]string)
	assets, _ := value.Details["assets"].([]string)
	if !slices.Equal(files, []string{"geo.bin"}) || !slices.Equal(assets, []string{"app/geo@1", "app/geo@2"}) {
		t.Fatalf("unexpected details: %#v", value.Details)
	}
	// Force is permission to replace what is on disk, not to lose one of two
	// files this command was told to write.
	if code := fault.As(unpackClash(t, service, archive, directory, true)).Code; code != "unpack_name_collision" {
		t.Fatalf("--force wrote through a name collision: %q", code)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused unpack wrote %d entries", len(entries))
	}

	// Naming one of them is the way through, since one asset cannot collide
	// with itself.
	result, err := service.Unpack(context.Background(), application.UnpackOptions{
		Pack:      archive,
		Directory: directory,
		Assets:    []application.Selection{application.ExactSelection(coord.MustParse("app/geo@2"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if written, readErr := os.ReadFile(filepath.Join(directory, "geo.bin")); readErr != nil ||
		!bytes.Equal(written, []byte("geo two bytes")) {
		t.Fatalf("the narrowed unpack wrote %q: %v", written, readErr)
	}
	if result.FileCount != 1 || result.ItemCount != 2 {
		t.Fatalf("unexpected counts: %#v", result)
	}

	// So is --tree, which is the only way to get both with their digests
	// checked.
	if _, err := service.Unpack(context.Background(), application.UnpackOptions{
		Pack: archive, Directory: t.TempDir(), Tree: true,
	}); err != nil {
		t.Fatalf("--tree refused an archive it can lay out: %v", err)
	}
}

// unpackClash runs the unpack the collision test refuses, so the with- and
// without-force cases read as the one difference between them.
func unpackClash(t *testing.T, service *application.Service, archive, directory string, force bool) error {
	t.Helper()
	_, err := service.Unpack(context.Background(), application.UnpackOptions{
		Pack: archive, Directory: directory, Force: force,
	})
	return err
}

// TestUnpackWritesOnlyTheAssetsSelected covers narrowing. A dacpack carries a
// whole project, and what reaches into one usually wants one asset out of it.
func TestUnpackWritesOnlyTheAssetsSelected(t *testing.T) {
	archive, contents := packedArchive(t)
	directory := t.TempDir()

	result, err := application.New("", "", nil, nil, nil).Unpack(context.Background(), application.UnpackOptions{
		Pack:      archive,
		Directory: directory,
		Assets:    []application.Selection{application.ExactSelection(coord.MustParse("java/sdk@11"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The counts are what tell a caller it got part of an archive rather than
	// all of a smaller one.
	if result.FileCount != 1 || result.ItemCount != 2 || result.ByteCount != int64(len(contents["jdk-11.tar.gz"])) {
		t.Fatalf("unexpected unpack result: %#v", result)
	}
	if len(result.Files) != 1 || result.Files[0].Coordinate != "java/sdk@11" {
		t.Fatalf("unexpected file report: %#v", result.Files)
	}
	written, err := os.ReadFile(filepath.Join(directory, "jdk-11.tar.gz"))
	if err != nil || !bytes.Equal(written, contents["jdk-11.tar.gz"]) {
		t.Fatalf("the unpack wrote %q: %v", written, err)
	}
	// The other version stays in the archive, which is the whole point.
	if _, err := os.Stat(filepath.Join(directory, "jdk-17.tar.gz")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the unpack materialized an asset nobody named: %v", err)
	}
}

// TestUnpackNarrowedByAssetTakesEveryVersion covers the shorter spelling, which
// is the one info and pull already read.
func TestUnpackNarrowedByAssetTakesEveryVersion(t *testing.T) {
	archive, contents := packedArchive(t)
	directory := t.TempDir()

	result, err := application.New("", "", nil, nil, nil).Unpack(context.Background(), application.UnpackOptions{
		Pack:      archive,
		Directory: directory,
		Assets:    []application.Selection{application.GroupSelection(coord.MustParse("java/sdk@11").Group())},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FileCount != 2 {
		t.Fatalf("a group selection wrote %d files", result.FileCount)
	}
	for name, content := range contents {
		if written, readErr := os.ReadFile(filepath.Join(directory, name)); readErr != nil || !bytes.Equal(written, content) {
			t.Fatalf("%s holds %q: %v", name, written, readErr)
		}
	}
}

// TestUnpackRefusesAnAssetTheDacpackDoesNotCarry keeps a typo from being read
// as "write nothing and report success". The refusal names the archive, because
// unpack reads no project and reporting one would point at a file it never
// opened.
func TestUnpackRefusesAnAssetTheDacpackDoesNotCarry(t *testing.T) {
	archive, _ := packedArchive(t)
	directory := t.TempDir()

	for _, selection := range []application.Selection{
		application.ExactSelection(coord.MustParse("java/sdk@21")),
		application.GroupSelection(coord.MustParse("python/sdk@3").Group()),
	} {
		_, err := application.New("", "", nil, nil, nil).Unpack(context.Background(), application.UnpackOptions{
			Pack: archive, Directory: directory, Assets: []application.Selection{selection},
		})
		value := fault.As(err)
		if value.Code != "asset_unknown" || !strings.Contains(value.Message, "dacpack") {
			t.Fatalf("unexpected refusal: %q %q (%v)", value.Code, value.Message, err)
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a refused unpack wrote %d entries", len(entries))
	}
}

// TestUnpackNarrowedStillChecksTheWholeArchive is what narrowing does not buy.
// Whether a dacpack is sound is not a question a command taking one asset out
// of it gets to skip, so an entry the index never listed still refuses the
// unpack -- even when the asset that was asked for read cleanly first.
func TestUnpackNarrowedStillChecksTheWholeArchive(t *testing.T) {
	content := []byte("the good file")
	archive := filepath.Join(t.TempDir(), "hostile.dacpack")
	writePackArchive(t, archive, map[string]any{
		"schemaVersion": 1,
		"items": []any{map[string]any{
			"coordinate": "app/geo@1",
			"sourceUrl":  "https://example.com/geo.bin",
			"file":       "assets/app/geo/1/geo.bin",
			"filename":   "geo.bin",
			"digest":     digest.Bytes(content),
			"size":       len(content),
		}},
	}, [][2]any{
		{"assets/app/geo/1/geo.bin", content},
		{"assets/../../../tmp/pwned", content},
	})

	directory := t.TempDir()
	_, err := application.New("", "", nil, nil, nil).Unpack(context.Background(), application.UnpackOptions{
		Pack:      archive,
		Directory: directory,
		Assets:    []application.Selection{application.ExactSelection(coord.MustParse("app/geo@1"))},
	})
	if code := fault.As(err).Code; code != "dacpack_invalid" {
		t.Fatalf("expected dacpack_invalid, got %q (%v)", code, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a rejected dacpack left %d entries behind", len(entries))
	}
}

// TestUnpackNarrowedIgnoresACollisionItWillNotCause covers what the destination
// check is asked about. An unpack that writes one asset is in nobody's way over
// the other two, and refusing it would make --force the answer to a file this
// command was never going to touch.
func TestUnpackNarrowedIgnoresACollisionItWillNotCause(t *testing.T) {
	archive, _ := packedArchive(t)
	directory := t.TempDir()
	occupied := filepath.Join(directory, "jdk-17.tar.gz")
	existing := []byte("something somebody was working on")
	if err := os.WriteFile(occupied, existing, 0o644); err != nil {
		t.Fatal(err)
	}

	service := application.New("", "", nil, nil, nil)
	if _, err := service.Unpack(context.Background(), application.UnpackOptions{
		Pack:      archive,
		Directory: directory,
		Assets:    []application.Selection{application.ExactSelection(coord.MustParse("java/sdk@11"))},
	}); err != nil {
		t.Fatal(err)
	}
	if kept, err := os.ReadFile(occupied); err != nil || !bytes.Equal(kept, existing) {
		t.Fatalf("the unpack changed a file it was not asked to write: %q %v", kept, err)
	}
	// The same file is in the way of the version it belongs to, and that unpack
	// is still refused.
	_, err := service.Unpack(context.Background(), application.UnpackOptions{
		Pack:      archive,
		Directory: directory,
		Assets:    []application.Selection{application.ExactSelection(coord.MustParse("java/sdk@17"))},
	})
	if code := fault.As(err).Code; code != "unpack_destination_occupied" {
		t.Fatalf("expected unpack_destination_occupied, got %q (%v)", code, err)
	}
}

// TestUnpackRefusesToReplaceWithoutForce covers the destination the command
// defaults to. Materializing lands real files in a directory somebody is
// standing in, and an archive is not a good enough reason to overwrite work
// that was never DAC's to lose.
func TestUnpackRefusesToReplaceWithoutForce(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	source := cache.New(t.TempDir())
	warmStore(t, source, content)
	archive := filepath.Join(t.TempDir(), "project.dacpack")
	if _, err := application.New(manifestPath, lockPath, source, nil, nil).Pack(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	service := application.New("", "", nil, nil, nil)

	directory := t.TempDir()
	target := filepath.Join(directory, "asset")
	existing := []byte("something somebody was working on")
	if err := os.WriteFile(target, existing, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := service.Unpack(context.Background(), application.UnpackOptions{Pack: archive, Directory: directory})
	if code := fault.As(err).Code; code != "unpack_destination_occupied" {
		t.Fatalf("expected unpack_destination_occupied, got %q (%v)", code, err)
	}
	// A refusal writes nothing, including the files that would not have collided.
	if kept, readErr := os.ReadFile(target); readErr != nil || !bytes.Equal(kept, existing) {
		t.Fatalf("the refusal changed the file: %q %v", kept, readErr)
	}

	result, err := service.Unpack(context.Background(), application.UnpackOptions{
		Pack: archive, Directory: directory, Force: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FileCount != 1 {
		t.Fatalf("--force wrote %d files", result.FileCount)
	}
	if written, readErr := os.ReadFile(target); readErr != nil || !bytes.Equal(written, content) {
		t.Fatalf("--force left %q: %v", written, readErr)
	}
}

// TestUnpackDoesNotWriteThroughASymlink is why the destination check uses
// Lstat. A link where a file is going is something already there, and following
// it would write outside the directory the caller named -- past every check
// that made sure the paths stayed inside it.
func TestUnpackDoesNotWriteThroughASymlink(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	source := cache.New(t.TempDir())
	warmStore(t, source, content)
	archive := filepath.Join(t.TempDir(), "project.dacpack")
	if _, err := application.New(manifestPath, lockPath, source, nil, nil).Pack(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	service := application.New("", "", nil, nil, nil)

	directory := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	untouched := []byte("a file the archive never named")
	if err := os.WriteFile(outside, untouched, 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(directory, "asset")
	if err := os.Symlink(outside, target); err != nil {
		t.Fatal(err)
	}

	// Without --force the link counts as occupied rather than as empty space.
	_, err := service.Unpack(context.Background(), application.UnpackOptions{Pack: archive, Directory: directory})
	if code := fault.As(err).Code; code != "unpack_destination_occupied" {
		t.Fatalf("a symlink was not treated as occupied: %q (%v)", code, err)
	}
	// With it, the link is replaced rather than followed.
	if _, err := service.Unpack(context.Background(), application.UnpackOptions{
		Pack: archive, Directory: directory, Force: true,
	}); err != nil {
		t.Fatal(err)
	}
	if kept, readErr := os.ReadFile(outside); readErr != nil || !bytes.Equal(kept, untouched) {
		t.Fatalf("the unpack wrote through the link: %q %v", kept, readErr)
	}
	info, err := os.Lstat(target)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("the link survived the unpack: %#v %v", info, err)
	}
}

// writePackArchive writes an archive from a literal index and literal entries,
// which is what a hostile dacpack is: a file somebody wrote by hand.
func writePackArchive(t *testing.T, path string, index any, entries [][2]any) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	writer := tar.NewWriter(file)
	data, err := json.Marshal(index)
	if err != nil {
		t.Fatal(err)
	}
	add := func(name string, content []byte) {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0o444, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	add("index.json", data)
	for _, entry := range entries {
		add(entry[0].(string), entry[1].([]byte))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestUnpackRefusesAPathItDidNotDerive is the check a digest layout got for
// free. Where every path is spelled by a digest, an index cannot claim one that
// points anywhere else. A dacpack's paths carry names a remote server chose, so
// the derivation has to be redone here and the claim compared against it --
// otherwise an index naming "../../../etc/cron.d/root" is a file write wherever
// DAC was run.
func TestUnpackRefusesAPathItDidNotDerive(t *testing.T) {
	content := []byte("evil payload")
	value := digest.Bytes(content)
	item := func(overrides map[string]any) map[string]any {
		base := map[string]any{
			"coordinate": "app/evil@1",
			"sourceUrl":  "https://evil.example.com/x",
			"file":       "assets/app/evil/1/x.bin",
			"filename":   "x.bin",
			"digest":     value,
			"size":       len(content),
		}
		for key, replacement := range overrides {
			base[key] = replacement
		}
		return base
	}
	for _, testCase := range []struct {
		reason  string
		item    map[string]any
		entries [][2]any
	}{
		{
			reason:  "an index claiming a path outside the asset root",
			item:    item(map[string]any{"file": "../../../tmp/pwned"}),
			entries: [][2]any{{"../../../tmp/pwned", content}},
		},
		{
			reason:  "an index claiming an absolute path",
			item:    item(map[string]any{"file": "/etc/cron.d/root"}),
			entries: [][2]any{{"/etc/cron.d/root", content}},
		},
		{
			reason: "a file name that is not one path element",
			item: item(map[string]any{
				"filename": "../../etc/passwd",
				"file":     "assets/app/evil/1/../../etc/passwd",
			}),
			entries: [][2]any{{"assets/app/evil/1/../../etc/passwd", content}},
		},
		{
			reason:  "a file name the archive and the index disagree about",
			item:    item(map[string]any{"filename": "x.bin"}),
			entries: [][2]any{{"assets/app/evil/1/other.bin", content}},
		},
		{
			// The index is clean and the archive smuggles the path instead.
			reason:  "an archive entry the index does not list",
			item:    item(nil),
			entries: [][2]any{{"assets/app/evil/1/x.bin", content}, {"assets/../../../tmp/pwned", content}},
		},
	} {
		archive := filepath.Join(t.TempDir(), "hostile.dacpack")
		writePackArchive(t, archive, map[string]any{
			"schemaVersion": 1,
			"items":         []any{testCase.item},
		}, testCase.entries)

		_, err := application.New("", "", nil, nil, nil).Unpack(context.Background(), application.UnpackOptions{Pack: archive, Directory: t.TempDir()})
		if code := fault.As(err).Code; code != "dacpack_invalid" {
			t.Fatalf("%s reported %q (%v)", testCase.reason, code, err)
		}
	}
}

// TestUnpackLeavesNothingBehindWhenItFails covers the failure that arrives too
// late to refuse up front. An archive is only known to be sound once it has
// been read to the end, so an entry the index never listed can turn up after
// good files have already been written -- and a half-materialized tree is the
// exact failure this command is built to avoid, because it looks complete to
// whatever reads it next.
func TestUnpackLeavesNothingBehindWhenItFails(t *testing.T) {
	first, second := []byte("the good file"), []byte("the smuggled file")
	value := digest.Bytes(first)
	archive := filepath.Join(t.TempDir(), "hostile.dacpack")
	writePackArchive(t, archive, map[string]any{
		"schemaVersion": 1,
		"items": []any{map[string]any{
			"coordinate": "app/geo@1",
			"sourceUrl":  "https://example.com/geo.bin",
			"file":       "assets/app/geo/1/geo.bin",
			"filename":   "geo.bin",
			"digest":     value,
			"size":       len(first),
		}},
	}, [][2]any{
		// A valid file, written before anything knows the archive is bad.
		{"assets/app/geo/1/geo.bin", first},
		// Then an entry the index never listed.
		{"assets/../../../tmp/pwned", second},
	})

	directory := t.TempDir()
	_, err := application.New("", "", nil, nil, nil).Unpack(context.Background(), application.UnpackOptions{
		Pack: archive, Directory: directory,
	})
	if code := fault.As(err).Code; code != "dacpack_invalid" {
		t.Fatalf("expected dacpack_invalid, got %q (%v)", code, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("a rejected dacpack left %v behind", names)
	}
}

// TestUnpackKeepsWhatItDidNotCreateWhenItFails is the other half of the
// rollback. Undoing an unpack means taking back what it added, and a directory
// that was already there is not that -- neither is anything else in it.
//
// The destination here is two levels below one that exists, which is the only
// way a flat unpack makes a directory at all: it is --dest naming somewhere
// that is not there yet. Those two come back; the one holding them does not.
func TestUnpackKeepsWhatItDidNotCreateWhenItFails(t *testing.T) {
	content := []byte("the good file")
	archive := filepath.Join(t.TempDir(), "hostile.dacpack")
	writePackArchive(t, archive, map[string]any{
		"schemaVersion": 1,
		"items": []any{map[string]any{
			"coordinate": "app/geo@1",
			"sourceUrl":  "https://example.com/geo.bin",
			"file":       "assets/app/geo/1/geo.bin",
			"filename":   "geo.bin",
			"digest":     digest.Bytes(content),
			"size":       len(content),
		}},
	}, [][2]any{
		{"assets/app/geo/1/geo.bin", content},
		{"assets/../../../tmp/pwned", content},
	})

	existing := t.TempDir()
	neighbour := filepath.Join(existing, "notes.txt")
	kept := []byte("somebody else's file")
	if err := os.WriteFile(neighbour, kept, 0o644); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(existing, "made", "by", "the", "unpack")

	if _, err := application.New("", "", nil, nil, nil).Unpack(context.Background(), application.UnpackOptions{
		Pack: archive, Directory: directory,
	}); err == nil {
		t.Fatal("a hostile dacpack was accepted")
	}
	if survived, err := os.ReadFile(neighbour); err != nil || !bytes.Equal(survived, kept) {
		t.Fatalf("the rollback removed a file it did not write: %q %v", survived, err)
	}
	if _, err := os.Stat(filepath.Join(existing, "made")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the rollback left the directories it created: %v", err)
	}
	entries, err := os.ReadDir(existing)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "notes.txt" {
		t.Fatalf("the rollback left %#v", entries)
	}
}

// TestUnpackRefusesContentThatDoesNotMatchTheIndex keeps the digest the thing
// that decides. A materialized file's name says nothing about its bytes, so the
// index's digest is the only claim there is to check.
func TestUnpackRefusesContentThatDoesNotMatchTheIndex(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	source := cache.New(t.TempDir())
	warmStore(t, source, content)
	archive := filepath.Join(t.TempDir(), "project.dacpack")
	if _, err := application.New(manifestPath, lockPath, source, nil, nil).Pack(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	position := bytes.Index(data, content)
	if position < 0 {
		t.Fatal("the archive does not contain the object")
	}
	copy(data[position:position+len(content)], []byte("other bytes"))
	if err := os.WriteFile(archive, data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = application.New("", "", nil, nil, nil).Unpack(context.Background(), application.UnpackOptions{Pack: archive, Directory: t.TempDir()})
	if code := fault.As(err).Code; code != "dacpack_invalid" {
		t.Fatalf("expected dacpack_invalid, got %q (%v)", code, err)
	}
}

// TestUnpackRefusesAnUnsupportedSchemaVersion keeps a dacpack from being read
// as something it is not. The schema is an integer nobody should guess the
// meaning of, so a version DAC does not know is refused rather than tried.
func TestUnpackRefusesAnUnsupportedSchemaVersion(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "future.dacpack")
	writePackArchive(t, archive, map[string]any{"schemaVersion": 2, "items": []any{}}, nil)
	_, err := application.New("", "", nil, nil, nil).Unpack(context.Background(), application.UnpackOptions{Pack: archive, Directory: t.TempDir()})
	if code := fault.As(err).Code; code != "dacpack_invalid" {
		t.Fatalf("expected dacpack_invalid, got %q (%v)", code, err)
	}
}

// TestPackRequiresACompleteCache covers what a half-written archive would
// cost: one missing an asset it lists is worse than no archive at all, because
// whoever receives it finds out at the point they needed the file.
func TestPackRequiresACompleteCache(t *testing.T) {
	manifestPath, lockPath := lockedProject(t, []byte("asset bytes"))
	service := application.New(manifestPath, lockPath, cache.New(t.TempDir()), nil, nil)
	_, err := service.Pack(context.Background(), filepath.Join(t.TempDir(), "project.dacpack"))
	if code := fault.As(err).Code; code != "cache_object_invalid" {
		t.Fatalf("expected cache_object_invalid, got %q (%v)", code, err)
	}
}
