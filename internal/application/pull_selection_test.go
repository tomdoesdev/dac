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
