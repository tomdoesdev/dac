package application_test

// Tests for the materialized archive. What separates a dacpack from a cache
// bundle is that its paths carry names an origin chose rather than digests DAC
// computed, so most of what is worth testing here is about those names: that
// they survive the round trip, that two assets cannot land on one path, and
// that a name arriving from outside cannot become a path DAC follows.

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/cache"
	"github.com/tom/dac/internal/coord"
	"github.com/tom/dac/internal/digest"
	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/project"
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

// TestPackMaterializesEachAssetUnderItsOwnName covers the difference from a
// cache bundle. Extracting this archive with tar has to leave a directory of
// real files with real extensions, because that is the only reason to prefer it
// over the bundle that names everything by digest.
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
// pays. Two coordinates that resolved to the same object are one blob in a
// cache bundle and two files here, because each is materialized under its own
// name and a file cannot be in two places.
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

// TestUnpackInstallsWhatPackWrote is the round trip. Unpack puts objects into
// the cache rather than onto the filesystem, so what it leaves behind is a
// cache a pull can answer from without a request.
func TestUnpackInstallsWhatPackWrote(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	source := cache.New(t.TempDir())
	warmStore(t, source, content)
	archive := filepath.Join(t.TempDir(), "project.dacpack")
	if _, err := application.New(manifestPath, lockPath, source, nil, nil).Pack(context.Background(), archive); err != nil {
		t.Fatal(err)
	}

	target := cache.New(t.TempDir())
	result, err := application.New("", "", target, nil, nil).Unpack(context.Background(), archive)
	if err != nil {
		t.Fatal(err)
	}
	if result.Pack != archive || result.ItemCount != 1 || result.ObjectCount != 1 || result.ByteCount != int64(len(content)) {
		t.Fatalf("unexpected unpack result: %#v", result)
	}
	object, found, err := target.Stat(digest.Bytes(content))
	if err != nil || !found || object.Size != int64(len(content)) {
		t.Fatalf("the unpack did not install the object: %#v %t %v", object, found, err)
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

// TestUnpackRefusesAPathItDidNotDerive is the check a cache bundle gets for
// free. Every path in a bundle is spelled by a digest, so an index cannot claim
// one that points anywhere else. A dacpack's paths carry names a remote server
// chose, so the derivation has to be redone here and the claim compared against
// it -- otherwise an index naming "../../../etc/cron.d/root" is a file write
// wherever DAC was run.
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

		_, err := application.New("", "", cache.New(t.TempDir()), nil, nil).Unpack(context.Background(), archive)
		if code := fault.As(err).Code; code != "dacpack_invalid" {
			t.Fatalf("%s reported %q (%v)", testCase.reason, code, err)
		}
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

	_, err = application.New("", "", cache.New(t.TempDir()), nil, nil).Unpack(context.Background(), archive)
	if code := fault.As(err).Code; code != "dacpack_invalid" {
		t.Fatalf("expected dacpack_invalid, got %q (%v)", code, err)
	}
}

// TestUnpackRefusesAnUnsupportedSchemaVersion keeps a dacpack from being read
// as something it is not. The bundle schema and this one both start at an
// integer nobody should guess the meaning of.
func TestUnpackRefusesAnUnsupportedSchemaVersion(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "future.dacpack")
	writePackArchive(t, archive, map[string]any{"schemaVersion": 2, "items": []any{}}, nil)
	_, err := application.New("", "", cache.New(t.TempDir()), nil, nil).Unpack(context.Background(), archive)
	if code := fault.As(err).Code; code != "dacpack_invalid" {
		t.Fatalf("expected dacpack_invalid, got %q (%v)", code, err)
	}
}

// TestPackRequiresACompleteCache refuses for the same reason export does: an
// archive missing an asset it lists is worse than no archive, because whoever
// receives it finds out at the point they needed the file.
func TestPackRequiresACompleteCache(t *testing.T) {
	manifestPath, lockPath := lockedProject(t, []byte("asset bytes"))
	service := application.New(manifestPath, lockPath, cache.New(t.TempDir()), nil, nil)
	_, err := service.Pack(context.Background(), filepath.Join(t.TempDir(), "project.dacpack"))
	if code := fault.As(err).Code; code != "cache_object_invalid" {
		t.Fatalf("expected cache_object_invalid, got %q (%v)", code, err)
	}
}
