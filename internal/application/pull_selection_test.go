package application_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/dac/internal/projecttest"
)

// TestPullTakesOnlyTheSelectedAssets covers the filter a narrowed pull applies.
// A project holds every asset every job needs, and a job needs the ones it
// needs.
func TestPullTakesOnlyTheSelectedAssets(t *testing.T) {
	manifestPath, lockPath, contents := multiAssetProject(t)
	store := newFakeStore()
	fetched := map[string]int{}
	fetcher := &fakeFetcher{fetch: func(_ context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
		content, known := contents[request.URL]
		if !known {
			return nil, errors.New("unexpected url " + request.URL)
		}
		fetched[request.URL]++
		return &application.FetchResponse{Length: int64(len(content)), Body: readCloser(content)}, nil
	}}
	service := application.New(manifestPath, lockPath, store, fetcher, nil)

	result, err := service.Pull(context.Background(), application.PullOptions{
		Concurrency: 2,
		Assets:      []application.Selection{application.ExactSelection(at("geo@1"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AssetCount != 1 || result.ProjectCount != 3 {
		t.Fatalf("unexpected counts: %#v", result)
	}
	if len(fetched) != 1 || fetched["https://example.com/geo-1"] != 1 {
		t.Fatalf("pull fetched %v, want only geo-1", fetched)
	}
}

// TestPullTakesEveryVersionOfASelectedAsset covers the shorter spelling, where
// a bare namespace/name names an asset rather than one of its versions.
func TestPullTakesEveryVersionOfASelectedAsset(t *testing.T) {
	manifestPath, lockPath, contents := multiAssetProject(t)
	fetched := map[string]int{}
	var fetchedMutex sync.Mutex
	fetcher := &fakeFetcher{fetch: func(_ context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
		content := contents[request.URL]
		fetchedMutex.Lock()
		fetched[request.URL]++
		fetchedMutex.Unlock()
		return &application.FetchResponse{Length: int64(len(content)), Body: readCloser(content)}, nil
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)

	result, err := service.Pull(context.Background(), application.PullOptions{
		Concurrency: 2,
		Assets:      []application.Selection{application.GroupSelection(at("geo@1").Group())},
	})
	if err != nil {
		t.Fatal(err)
	}
	fetchedMutex.Lock()
	defer fetchedMutex.Unlock()
	if result.AssetCount != 2 || len(fetched) != 2 {
		t.Fatalf("unexpected result: count=%d fetched=%v", result.AssetCount, fetched)
	}
}

// TestPullFetchesAnOverlappingSelectionOnce covers a coordinate named twice,
// once whole and once by its asset. A selection is a filter over the project
// rather than a list of fetches to run.
func TestPullFetchesAnOverlappingSelectionOnce(t *testing.T) {
	manifestPath, lockPath, contents := multiAssetProject(t)
	fetched := map[string]int{}
	fetcher := &fakeFetcher{fetch: func(_ context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
		content := contents[request.URL]
		fetched[request.URL]++
		return &application.FetchResponse{Length: int64(len(content)), Body: readCloser(content)}, nil
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)

	result, err := service.Pull(context.Background(), application.PullOptions{
		Concurrency: 1,
		Assets: []application.Selection{
			application.GroupSelection(at("geo@1").Group()),
			application.ExactSelection(at("geo@1")),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.AssetCount != 2 {
		t.Fatalf("unexpected asset count: %#v", result)
	}
	for url, count := range fetched {
		if count != 1 {
			t.Fatalf("%s was fetched %d times", url, count)
		}
	}
}

// TestPullRefusesAnUnknownSelection keeps a typo from being read as "fetch
// nothing and report success".
func TestPullRefusesAnUnknownSelection(t *testing.T) {
	manifestPath, lockPath, _ := multiAssetProject(t)
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher([]byte("x")), nil)

	_, err := service.Pull(context.Background(), application.PullOptions{
		Concurrency: 1,
		Assets:      []application.Selection{application.ExactSelection(at("geo@9"))},
	})
	if code := fault.As(err).Code; code != "asset_unknown" {
		t.Fatalf("expected asset_unknown, got %q (%v)", code, err)
	}
}

// TestNarrowedRefreshReachesOnlyTheNamedOrigins covers what naming assets buys a
// refresh. The lock file it writes still describes the whole project, because one
// that did not would be stale the moment it landed, but the assets it did not name
// are settled from the entries the lock already holds rather than from their origins.
func TestNarrowedRefreshReachesOnlyTheNamedOrigins(t *testing.T) {
	manifestPath, lockPath, contents := multiAssetProject(t)
	moved := []byte("kit bytes, moved")

	fetched := map[string]int{}
	fetcher := &fakeFetcher{fetch: func(_ context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
		fetched[request.URL]++
		return &application.FetchResponse{Length: int64(len(moved)), Body: readCloser(moved)}, nil
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)

	result, err := service.Pull(context.Background(), application.PullOptions{
		Concurrency: 1, MaxSize: 1 << 20, Refresh: true,
		Assets: []application.Selection{application.ExactSelection(at("kit@1"))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fetched) != 1 || fetched["https://example.com/kit"] != 1 {
		t.Fatalf("the refresh fetched %v, want only the asset it named", fetched)
	}
	if len(result.Locked) != 1 || result.Locked[0] != "test/kit@1" || !result.Changed {
		t.Fatalf("unexpected lock work: %#v", result)
	}
	updated, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Assets[at("kit@1")].Digest != digest.Bytes(moved) {
		t.Fatal("the named asset did not take the bytes its origin now serves")
	}
	if updated.Assets[at("geo@1")].Digest != digest.Bytes(contents["https://example.com/geo-1"]) {
		t.Fatal("an asset the refresh did not name lost the bytes the lock described")
	}
	// The file that lands describes the manifest whole, which is the only kind a later run accepts.
	projecttest.Check(t, manifestPath, lockPath)
}

// A validator already learned for an asset remains a useful upstream-check hint
// after that asset is pinned, even when a narrowed refresh carries it through.
func TestNarrowedRefreshKeepsAStoredETagOnceAnAssetIsPinned(t *testing.T) {
	manifestPath, lockPath, contents := multiAssetProject(t)
	pin(t, manifestPath, at("geo@1"), digest.Bytes(contents["https://example.com/geo-1"]))

	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	locked := lock.Assets[at("geo@1")]
	locked.ETag = "\"stale\""
	lock.Assets[at("geo@1")] = locked
	if err := project.Write(lockPath, lock); err != nil {
		t.Fatal(err)
	}

	refreshOnlyKit(t, manifestPath, lockPath, contents)
	updated, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if tag := updated.Assets[at("geo@1")].ETag; tag != "\"stale\"" {
		t.Fatalf("the carried-through pin changed ETag to %q", tag)
	}
}

// A lock from before the file name field existed gains its names the next time
// anything writes the file, and gains them without a request: the URL already holds
// the answer, and so does a name the manifest declares.
func TestNarrowedRefreshSettlesNamesWithoutARequest(t *testing.T) {
	manifestPath, lockPath, contents := multiAssetProject(t)
	manifest, err := project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	asset := manifest.Assets[at("geo@2")]
	asset.Filename = "geo.db"
	manifest.Assets[at("geo@2")] = asset
	writeManifest(t, manifestPath, manifest)

	refreshOnlyKit(t, manifestPath, lockPath, contents)
	updated, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	// The URL answers for the entry the manifest says nothing about.
	if name := updated.Assets[at("geo@1")].Filename; name != "geo-1" {
		t.Fatalf("the backfilled name is %q", name)
	}
	// A declared name is the project's own answer, and it beats what the URL spells.
	if name := updated.Assets[at("geo@2")].Filename; name != "geo.db" {
		t.Fatalf("the declared name is %q", name)
	}

	// Settling a name is a migration and happens once.
	before := projecttest.MustRead(t, lockPath)
	refreshOnlyKit(t, manifestPath, lockPath, contents)
	if !bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("a second refresh rewrote a lock file that had nothing to change")
	}
}

// pin records a publisher digest for one manifest asset, the way a hand edit or add --pin does.
func pin(t *testing.T, manifestPath string, name coord.Coordinate, integrity string) {
	t.Helper()
	manifest, err := project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	asset := manifest.Assets[name]
	asset.Integrity = integrity
	manifest.Assets[name] = asset
	writeManifest(t, manifestPath, manifest)
}

// refreshOnlyKit refreshes the one asset of multiAssetProject that these tests are not
// watching, and fails if it reaches any other origin. What it leaves behind in the lock
// file is what a reconcile settled without asking anybody.
func refreshOnlyKit(t *testing.T, manifestPath, lockPath string, contents map[string][]byte) {
	t.Helper()
	fetcher := &fakeFetcher{fetch: func(_ context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
		if request.URL != "https://example.com/kit" {
			t.Errorf("the refresh reached %s, which it did not name", request.URL)
		}
		content := contents["https://example.com/kit"]
		return &application.FetchResponse{Length: int64(len(content)), Body: readCloser(content)}, nil
	}}
	if _, err := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil).
		Pull(context.Background(), application.PullOptions{
			Concurrency: 1, MaxSize: 1 << 20, Refresh: true,
			Assets: []application.Selection{application.ExactSelection(at("kit@1"))},
		}); err != nil {
		t.Fatal(err)
	}
}

// multiAssetProject writes a locked project holding two versions of one asset
// and a second asset, which is the shape a filter has anything to say about.
func multiAssetProject(t *testing.T) (string, string, map[string][]byte) {
	t.Helper()
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")

	contents := map[string][]byte{
		"https://example.com/geo-1": []byte("geo one bytes"),
		"https://example.com/geo-2": []byte("geo two bytes"),
		"https://example.com/kit":   []byte("kit bytes"),
	}
	sources := map[coord.Coordinate]string{
		at("geo@1"): "https://example.com/geo-1",
		at("geo@2"): "https://example.com/geo-2",
		at("kit@1"): "https://example.com/kit",
	}
	manifest := project.Manifest{SchemaVersion: project.ManifestVersion, Assets: map[coord.Coordinate]project.Asset{}}
	locked := map[coord.Coordinate]project.LockAsset{}
	for name, url := range sources {
		manifest.Assets[name] = project.Asset{URL: url}
		locked[name] = project.LockAsset{
			URL:    url,
			Digest: digest.Bytes(contents[url]),
			Size:   int64(len(contents[url])),
		}
	}
	lock, err := project.NewLock(manifest, locked)
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WritePair(manifestPath, lockPath, manifest, lock); err != nil {
		t.Fatal(err)
	}
	return manifestPath, lockPath, contents
}

// readCloser wraps fixture bytes as a response body, the way every other
// fetcher in these tests does.
func readCloser(content []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(content))
}
