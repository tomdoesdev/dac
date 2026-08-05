package application_test

import (
	"bytes"
	"context"
	"errors"
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

type unpackFixture struct {
	coordinate       string
	content          []byte
	manifestFilename string
	lockFilename     string
	cached           bool
}

// unpackProject writes one project and installs the requested cache objects.
func unpackProject(t *testing.T, fixtures []unpackFixture) (string, string, *cache.Store) {
	t.Helper()
	root := t.TempDir()
	manifestPath := filepath.Join(root, "dac.json")
	lockPath := filepath.Join(root, "dac-lock.json")
	manifest := project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets:        make(map[coord.Coordinate]project.Asset, len(fixtures)),
	}
	locked := make(map[coord.Coordinate]project.LockAsset, len(fixtures))
	store := cache.New(filepath.Join(root, "cache"))
	for _, fixture := range fixtures {
		name := coord.MustParse(fixture.coordinate)
		url := "https://example.com/" + name.Name
		manifest.Assets[name] = project.Asset{URL: url, Filename: fixture.manifestFilename}
		locked[name] = project.LockAsset{
			URL: url, Digest: digest.Bytes(fixture.content), Size: int64(len(fixture.content)),
			Filename: fixture.lockFilename,
		}
		if fixture.cached {
			if _, err := store.Put(context.Background(), bytes.NewReader(fixture.content), application.PutAny("", 1<<20)); err != nil {
				t.Fatal(err)
			}
		}
	}
	lock, err := project.NewLock(manifest, locked)
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WritePair(manifestPath, lockPath, manifest, lock); err != nil {
		t.Fatal(err)
	}
	return manifestPath, lockPath, store
}

func TestUnpackSelectsExactGroupAndWholeProject(t *testing.T) {
	fixtures := []unpackFixture{
		{coordinate: "app/geo@1", content: []byte("geo one"), manifestFilename: "geo-1.bin", cached: true},
		{coordinate: "app/geo@2", content: []byte("geo two"), manifestFilename: "geo-2.bin", cached: true},
		{coordinate: "app/map@1", content: []byte("map"), manifestFilename: "map.bin", cached: true},
	}
	manifestPath, lockPath, store := unpackProject(t, fixtures)
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)
	tests := []struct {
		name       string
		selections []application.Selection
		files      []string
	}{
		{name: "exact", selections: []application.Selection{application.ExactSelection(coord.MustParse("app/geo@1"))}, files: []string{"geo-1.bin"}},
		{name: "group", selections: []application.Selection{application.GroupSelection(coord.MustParse("app/geo@1").Group())}, files: []string{"geo-1.bin", "geo-2.bin"}},
		{name: "whole project", files: []string{"geo-1.bin", "geo-2.bin", "map.bin"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			destination := filepath.Join(t.TempDir(), "output")
			result, err := service.Unpack(context.Background(), application.UnpackOptions{
				Directory: destination,
				Assets:    test.selections,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.ProjectCount != len(fixtures) || result.FileCount != len(test.files) {
				t.Fatalf("unexpected result counts: %#v", result)
			}
			var names []string
			for _, file := range result.Files {
				names = append(names, file.Filename)
				if file.Coordinate == "" || file.Path != filepath.Join(destination, file.Filename) || file.Digest == "" || file.Size == 0 {
					t.Fatalf("incomplete file result: %#v", file)
				}
			}
			slices.Sort(names)
			if !slices.Equal(names, test.files) {
				t.Fatalf("files are %v, want %v", names, test.files)
			}
		})
	}
}

func TestUnpackUsesFilenamePrecedence(t *testing.T) {
	fixtures := []unpackFixture{
		{coordinate: "app/declared@1", content: []byte("declared"), manifestFilename: "manifest.bin", lockFilename: "lock.bin", cached: true},
		{coordinate: "app/resolved@1", content: []byte("resolved"), lockFilename: "resolved.bin", cached: true},
		{coordinate: "app/fallback@1", content: []byte("fallback"), cached: true},
	}
	manifestPath, lockPath, store := unpackProject(t, fixtures)
	destination := filepath.Join(t.TempDir(), "output")
	result, err := application.New(manifestPath, lockPath, store, nil, nil).Unpack(
		context.Background(), application.UnpackOptions{Directory: destination})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range result.Files {
		names = append(names, file.Filename)
	}
	slices.Sort(names)
	if want := []string{"fallback", "manifest.bin", "resolved.bin"}; !slices.Equal(names, want) {
		t.Fatalf("file names are %v, want %v", names, want)
	}
}

func TestUnpackRejectsAllFilenameCollisionsBeforeWriting(t *testing.T) {
	content := []byte("shared")
	fixtures := []unpackFixture{
		{coordinate: "app/geo@1", content: content, lockFilename: "same.bin", cached: true},
		{coordinate: "app/geo@2", content: content, lockFilename: "same.bin", cached: true},
	}
	manifestPath, lockPath, store := unpackProject(t, fixtures)
	destination := filepath.Join(t.TempDir(), "output")
	_, err := application.New(manifestPath, lockPath, store, nil, nil).Unpack(
		context.Background(), application.UnpackOptions{Directory: destination})
	if fault.As(err).Code != "unpack_name_collision" {
		t.Fatalf("expected unpack_name_collision, got %v", err)
	}
	if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("collision created the destination: %v", statErr)
	}
}

func TestUnpackReportsMissingAndCorruptObjects(t *testing.T) {
	tests := []struct {
		name    string
		cached  bool
		corrupt bool
		code    string
	}{
		{name: "missing", code: "cache_object_invalid"},
		{name: "corrupt", cached: true, corrupt: true, code: "cache_object_corrupt"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			content := []byte("asset bytes")
			manifestPath, lockPath, store := unpackProject(t, []unpackFixture{{
				coordinate: "app/asset@1", content: content, lockFilename: "asset.bin", cached: test.cached,
			}})
			if test.corrupt {
				path, err := store.Path(digest.Bytes(content))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("other bytes"), 0o644); err != nil {
					t.Fatal(err)
				}
				store = cache.New(store.Root)
			}
			destination := filepath.Join(t.TempDir(), "output")
			_, err := application.New(manifestPath, lockPath, store, nil, nil).Unpack(
				context.Background(), application.UnpackOptions{Directory: destination})
			if value := fault.As(err); value.Code != test.code || !strings.Contains(value.Message, "dac pull") {
				t.Fatalf("unexpected error: %#v", value)
			}
			if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failure created the destination: %v", statErr)
			}
		})
	}
}

func TestUnpackCancelsWithoutAFile(t *testing.T) {
	manifestPath, lockPath, store := unpackProject(t, []unpackFixture{{
		coordinate: "app/asset@1", content: []byte("asset bytes"), lockFilename: "asset.bin", cached: true,
	}})
	destination := filepath.Join(t.TempDir(), "output")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := application.New(manifestPath, lockPath, store, nil, nil).Unpack(
		ctx, application.UnpackOptions{Directory: destination})
	if fault.As(err).Code != "cancelled" {
		t.Fatalf("expected cancelled, got %v", err)
	}
	if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("cancelled unpack left a destination: %v", statErr)
	}
}

func TestUnpackForceReplacesFilesWithoutFollowingSymlinks(t *testing.T) {
	content := []byte("new bytes")
	manifestPath, lockPath, store := unpackProject(t, []unpackFixture{{
		coordinate: "app/asset@1", content: content, lockFilename: "asset.bin", cached: true,
	}})
	destination := t.TempDir()
	path := filepath.Join(destination, "asset.bin")
	outside := filepath.Join(t.TempDir(), "outside.bin")
	if err := os.WriteFile(outside, []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	service := application.New(manifestPath, lockPath, store, nil, nil)
	if _, err := service.Unpack(context.Background(), application.UnpackOptions{Directory: destination}); fault.As(err).Code != "unpack_destination_occupied" {
		t.Fatalf("expected occupied destination, got %v", err)
	}
	if _, err := service.Unpack(context.Background(), application.UnpackOptions{Directory: destination, Force: true}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, content) {
		t.Fatalf("destination has %q: %v", data, err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "outside" {
		t.Fatalf("symlink target changed to %q: %v", data, err)
	}
}

func TestUnpackNeverReplacesDirectories(t *testing.T) {
	manifestPath, lockPath, store := unpackProject(t, []unpackFixture{{
		coordinate: "app/asset@1", content: []byte("asset"), lockFilename: "asset.bin", cached: true,
	}})
	destination := t.TempDir()
	if err := os.Mkdir(filepath.Join(destination, "asset.bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := application.New(manifestPath, lockPath, store, nil, nil).Unpack(
		context.Background(), application.UnpackOptions{Directory: destination, Force: true})
	if fault.As(err).Code != "unpack_destination_occupied" {
		t.Fatalf("expected occupied destination, got %v", err)
	}
}

func TestUnpackNeverFollowsTheDestinationDirectorySymlink(t *testing.T) {
	manifestPath, lockPath, store := unpackProject(t, []unpackFixture{{
		coordinate: "app/asset@1", content: []byte("asset"), lockFilename: "asset.bin", cached: true,
	}})
	realDestination := t.TempDir()
	destination := filepath.Join(t.TempDir(), "destination")
	if err := os.Symlink(realDestination, destination); err != nil {
		t.Fatal(err)
	}
	_, err := application.New(manifestPath, lockPath, store, nil, nil).Unpack(
		context.Background(), application.UnpackOptions{Directory: destination, Force: true})
	if fault.As(err).Code != "unpack_destination_occupied" {
		t.Fatalf("expected occupied destination, got %v", err)
	}
	entries, readErr := os.ReadDir(realDestination)
	if readErr != nil || len(entries) != 0 {
		t.Fatalf("unpack followed the destination symlink: %v %v", entries, readErr)
	}
}

type mutateOnStatStore struct {
	*cache.Store
	trigger     string
	path        string
	replacement []byte
}

// Stat changes one object after its final preflight check to test staged hashing.
func (store *mutateOnStatStore) Stat(value string) (application.Object, bool, error) {
	object, found, err := store.Store.Stat(value)
	if err == nil && found && value == store.trigger {
		if chmodErr := os.Chmod(store.path, 0o644); chmodErr != nil {
			return application.Object{}, false, chmodErr
		}
		if writeErr := os.WriteFile(store.path, store.replacement, 0o644); writeErr != nil {
			return application.Object{}, false, writeErr
		}
	}
	return object, found, err
}

func TestUnpackRemovesStagedFilesWhenHashingFails(t *testing.T) {
	first := []byte("first bytes")
	second := []byte("second bytes")
	manifestPath, lockPath, base := unpackProject(t, []unpackFixture{
		{coordinate: "app/first@1", content: first, lockFilename: "first.bin", cached: true},
		{coordinate: "app/second@1", content: second, lockFilename: "second.bin", cached: true},
	})
	secondPath, err := base.Path(digest.Bytes(second))
	if err != nil {
		t.Fatal(err)
	}
	store := &mutateOnStatStore{
		Store: base, trigger: digest.Bytes(second), path: secondPath, replacement: []byte("broken bytes"),
	}
	destination := filepath.Join(t.TempDir(), "output")
	_, err = application.New(manifestPath, lockPath, store, nil, nil).Unpack(
		context.Background(), application.UnpackOptions{Directory: destination})
	if fault.As(err).Code != "cache_object_corrupt" {
		t.Fatalf("expected cache_object_corrupt, got %v", err)
	}
	if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("failed staging left a destination: %v", statErr)
	}
}

func TestUnpackRejectsUnsafeLockFilenames(t *testing.T) {
	manifestPath, lockPath, store := unpackProject(t, []unpackFixture{{
		coordinate: "app/asset@1", content: []byte("asset"), lockFilename: "safe.bin", cached: true,
	}})
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte(`"filename": "safe.bin"`), []byte(`"filename": "../escape"`), 1)
	if err := os.WriteFile(lockPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(t.TempDir(), "output")
	_, err = application.New(manifestPath, lockPath, store, nil, nil).Unpack(
		context.Background(), application.UnpackOptions{Directory: destination})
	if fault.As(err).Code != "lock_invalid" {
		t.Fatalf("expected lock_invalid, got %v", err)
	}
	if _, statErr := os.Stat(destination); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("unsafe name created the destination: %v", statErr)
	}
}
