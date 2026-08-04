package application_test

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/cache"
	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/dac/internal/projecttest"
	"github.com/tomdoesdev/dac/internal/rewrite"
)

// at builds a test coordinate from its name and version. Every fixture shares
// one namespace, because these tests are about everything except which
// namespace an asset is in; the tests that are about that spell it out.
func at(text string) coord.Coordinate { return coord.MustParse("test/" + text) }

var (
	_ application.ObjectStore = (*fakeStore)(nil)
	_ application.Fetcher     = (*fakeFetcher)(nil)
	_ application.Reporter    = (*fakeReporter)(nil)
)

type fakeStore struct {
	mutex   sync.Mutex
	objects map[string][]byte
	// corrupt maps a digest to the digest its bytes actually have, so a test can
	// reproduce a damaged cache without going near a filesystem.
	corrupt map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{objects: map[string][]byte{}, corrupt: map[string]string{}}
}

func (*fakeStore) Path(value string) (string, error) {
	return filepath.Join("/cache", strings.TrimPrefix(value, digest.Prefix)), nil
}

// damage marks a stored object as holding bytes that hash to something else.
func (store *fakeStore) damage(value, actual string) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.corrupt[value] = actual
}

// damaged reports the corruption a store still holds for a digest.
func (store *fakeStore) damaged(value string) bool {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	_, exists := store.corrupt[value]
	return exists
}

// inspect reports the object a digest names, and the corruption recorded for
// it. The caller holds no lock.
func (store *fakeStore) inspect(value string) (application.Object, bool, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	content, exists := store.objects[value]
	if !exists {
		return application.Object{}, false, nil
	}
	if actual, damaged := store.corrupt[value]; damaged {
		return application.Object{Digest: value, Size: int64(len(content))}, true,
			&application.CorruptError{Digest: value, ActualDigest: actual, Path: "/cache/" + value}
	}
	return application.Object{Digest: value, Size: int64(len(content))}, true, nil
}

func (store *fakeStore) Stat(value string) (application.Object, bool, error) {
	object, exists, err := store.inspect(value)
	if err != nil {
		return application.Object{}, false, err
	}
	return object, exists, nil
}

func (store *fakeStore) Verify(_ context.Context, value string) (application.Object, bool, error) {
	return store.inspect(value)
}

// Describe reports an object without touching its liveness timestamp. The fake
// keeps no timestamps, so every object reports the zero time.
func (store *fakeStore) Describe(value string) (application.ObjectDescription, bool, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	content, exists := store.objects[value]
	if !exists {
		return application.ObjectDescription{}, false, nil
	}
	return application.ObjectDescription{Digest: value, Size: int64(len(content))}, true, nil
}

func (store *fakeStore) List(context.Context) ([]string, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	digests := make([]string, 0, len(store.objects))
	for value := range store.objects {
		digests = append(digests, value)
	}
	slices.Sort(digests)
	return digests, nil
}

func (store *fakeStore) Remove(_ context.Context, value string) error {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	delete(store.objects, value)
	delete(store.corrupt, value)
	return nil
}

// has reports whether the fake holds exactly this object.
func (store *fakeStore) has(object application.Object) bool {
	found, exists, err := store.Stat(object.Digest)
	return err == nil && exists && found.Size == object.Size
}

func (*fakeStore) WithLock(_ context.Context, _ string, operation func() error) error {
	return operation()
}

func (store *fakeStore) GC(_ context.Context, options application.GCOptions) (application.GCResult, error) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	result := application.GCResult{Digests: []string{}, DryRun: options.DryRun}
	for value, content := range store.objects {
		result.Digests = append(result.Digests, value)
		result.ObjectCount++
		result.ByteCount += int64(len(content))
		if !options.DryRun {
			delete(store.objects, value)
		}
	}
	slices.Sort(result.Digests)
	return result, nil
}

func (store *fakeStore) Put(ctx context.Context, reader io.Reader, options application.PutOptions) (application.Object, error) {
	content, err := io.ReadAll(reader)
	if err != nil {
		return application.Object{}, err
	}
	if err := ctx.Err(); err != nil {
		return application.Object{}, err
	}
	expect := options.Expect
	if expect.Size == application.Unknown && options.MaxSize > 0 && int64(len(content)) > options.MaxSize {
		return application.Object{}, application.ErrTooLarge
	}
	value := digest.Bytes(content)
	if expect.Size != application.Unknown && int64(len(content)) != expect.Size || expect.Digest != "" && expect.Digest != value {
		return application.Object{}, &application.ContentError{
			ExpectedDigest: expect.Digest,
			ActualDigest:   value,
			ExpectedSize:   expect.Size,
			ActualSize:     int64(len(content)),
		}
	}
	store.mutex.Lock()
	store.objects[value] = bytes.Clone(content)
	// An install writes known-good bytes, so it repairs whatever was there.
	delete(store.corrupt, value)
	store.mutex.Unlock()
	return application.Object{Digest: value, Size: int64(len(content))}, nil
}

type fakeFetcher struct {
	mutex    sync.Mutex
	requests []application.FetchRequest
	fetch    func(context.Context, application.FetchRequest) (*application.FetchResponse, error)
}

func (fetcher *fakeFetcher) Fetch(ctx context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
	fetcher.mutex.Lock()
	fetcher.requests = append(fetcher.requests, request)
	fetcher.mutex.Unlock()
	return fetcher.fetch(ctx, request)
}

func (fetcher *fakeFetcher) count() int {
	fetcher.mutex.Lock()
	defer fetcher.mutex.Unlock()
	return len(fetcher.requests)
}

type fakeReporter struct {
	mutex  sync.Mutex
	events []string
}

func (reporter *fakeReporter) Start(name string, total int64) {
	reporter.add(fmt.Sprintf("start:%s:%d", name, total))
}

func (reporter *fakeReporter) Advance(name string, count int64) {
	reporter.add(fmt.Sprintf("advance:%s:%d", name, count))
}

func (reporter *fakeReporter) Done(name, status string) {
	reporter.add("done:" + name + ":" + status)
}

func (reporter *fakeReporter) Fail(name string, err error) {
	reporter.add("fail:" + name + ":" + fault.As(err).Code)
}

func (reporter *fakeReporter) Wait() {
	reporter.add("wait")
}

func (reporter *fakeReporter) add(value string) {
	reporter.mutex.Lock()
	defer reporter.mutex.Unlock()
	reporter.events = append(reporter.events, value)
}

func TestAddAndRemoveKeepProjectFilesMatched(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	store := newFakeStore()
	fetcher := staticFetcher([]byte("one"))
	reporter := &fakeReporter{}
	service := application.New(manifestPath, lockPath, store, fetcher, reporter)

	added, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("geo@1"), URL: "https://example.com/one", MaxSize: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if added.Coordinate != "test/geo@1" || added.Namespace != "test" || added.Name != "geo" ||
		added.Version != "1" || !added.Cached {
		t.Fatalf("unexpected add result: %#v", added)
	}
	projecttest.Check(t, manifestPath, lockPath)
	if fetcher.count() != 1 {
		t.Fatalf("add made %d requests", fetcher.count())
	}

	beforeManifest := projecttest.MustRead(t, manifestPath)
	beforeLock := projecttest.MustRead(t, lockPath)
	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("geo@1"), URL: "https://example.com/two", MaxSize: 100,
	}); fault.As(err).Code != "asset_exists" {
		t.Fatalf("expected asset_exists, got %v", err)
	}
	assertFilesEqual(t, manifestPath, lockPath, beforeManifest, beforeLock)

	if _, err := service.Remove(at("geo@2")); fault.As(err).Code != "asset_unknown" {
		t.Fatalf("expected asset_unknown, got %v", err)
	}
	if _, err := service.Remove(at("geo@1")); err != nil {
		t.Fatal(err)
	}
	manifest, lock := projecttest.Check(t, manifestPath, lockPath)
	if len(manifest.Assets) != 0 || len(lock.Assets) != 0 {
		t.Fatalf("remove left assets: %#v %#v", manifest.Assets, lock.Assets)
	}
	if fetcher.count() != 1 {
		t.Fatal("remove made a network request")
	}
	if len(reporter.events) == 0 || reporter.events[len(reporter.events)-1] != "wait" {
		t.Fatalf("reporter did not receive its final wait: %v", reporter.events)
	}
}

func TestInfoCombinesProjectRequestAndCacheState(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	store := newFakeStore()
	warm(t, store, content)
	config, err := rewrite.Parse(strings.NewReader(
		"block *\nallow mirror.internal\nrewrite ^example\\.com/(.*)$ mirror.internal/$1\n"))
	if err != nil {
		t.Fatal(err)
	}
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)
	result, err := service.Info(application.InfoOptions{Selection: application.ExactSelection(at("asset@1")), Rewriter: config})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.AssetCount != 1 || result.Summary.CachedCount != 1 || result.Summary.CorruptCount != 0 ||
		result.Summary.BlockedCount != 0 || result.Summary.LockStatus != "current" {
		t.Fatalf("unexpected info counts: %#v", result.Summary)
	}
	asset := result.Assets[0]
	if asset.SourceURL != "https://example.com/asset" || asset.RequestURL != "https://mirror.internal/asset" ||
		!asset.Rewritten || asset.RequestStatus != "allowed" || asset.CacheStatus != "cached" ||
		asset.Digest != digest.Bytes(content) || asset.Size == nil || *asset.Size != int64(len(content)) || asset.Path == "" {
		t.Fatalf("unexpected info asset: %#v", asset)
	}
	_, err = service.Info(application.InfoOptions{Selection: application.ExactSelection(at("asset@2")), Rewriter: config})
	unknown := fault.As(err)
	if unknown.Code != "asset_unknown" {
		t.Fatalf("expected asset_unknown, got %v", unknown)
	}
	// The versions the project does have are the useful half of the answer: a
	// coordinate one character off and a name nobody ever added are the same
	// error otherwise.
	if versions, _ := unknown.Details["versions"].([]string); len(versions) != 1 || versions[0] != "1" {
		t.Fatalf("asset_unknown did not name the versions the project has: %#v", unknown.Details)
	}
}

// A version is part of an asset's identity, so a second version is a second
// entry. It needs no --force, because nothing that referred to the first
// coordinate stops working.
func TestAddKeepsBothVersionsOfAnAsset(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	service := application.New(manifestPath, lockPath, newFakeStore(), pathFetcher(), nil)
	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("geo@1"), URL: "https://example.com/one", MaxSize: 100,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("geo@2"), URL: "https://example.com/two", MaxSize: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Siblings) != 1 || result.Siblings[0] != "1" {
		t.Fatalf("add did not report the version already there: %#v", result.Siblings)
	}
	manifest, lock := projecttest.Check(t, manifestPath, lockPath)
	if len(manifest.Assets) != 2 || len(lock.Assets) != 2 {
		t.Fatalf("the project does not hold both versions: %#v %#v", manifest.Assets, lock.Assets)
	}
	if manifest.Assets[at("geo@1")].URL != "https://example.com/one" ||
		manifest.Assets[at("geo@2")].URL != "https://example.com/two" {
		t.Fatalf("the versions do not have their own sources: %#v", manifest.Assets)
	}
}

// Force is now only about one coordinate: it replaces the source of a version
// the manifest already has, and leaves every other version alone.
func TestAddForceReplacesOneVersionSource(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	service := application.New(manifestPath, lockPath, newFakeStore(), pathFetcher(), nil)
	for _, options := range []application.AddOptions{
		{Coordinate: at("geo@1"), URL: "https://example.com/one", MaxSize: 100},
		{Coordinate: at("geo@2"), URL: "https://example.com/two", MaxSize: 100},
	} {
		if _, err := service.Add(context.Background(), options); err != nil {
			t.Fatal(err)
		}
	}
	// A moved source that serves the same bytes is not a rebind, so replacing
	// one version's URL needs nothing beyond --force.
	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("geo@2"), URL: "https://mirror.example.com/two", Force: true, MaxSize: 100,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := projecttest.Check(t, manifestPath, lockPath)
	if len(manifest.Assets) != 2 {
		t.Fatalf("force changed the number of versions: %#v", manifest.Assets)
	}
	if manifest.Assets[at("geo@1")].URL != "https://example.com/one" ||
		manifest.Assets[at("geo@2")].URL != "https://mirror.example.com/two" {
		t.Fatalf("force replaced the wrong version: %#v", manifest.Assets)
	}
}

func TestAddResolvesOnlyTheChangedAsset(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	fetcher := staticFetcher([]byte("content"))
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)
	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("alpha@1"), URL: "https://example.com/alpha", MaxSize: 100,
	}); err != nil {
		t.Fatal(err)
	}
	firstLock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("beta@1"), URL: "https://example.com/beta", MaxSize: 100,
	}); err != nil {
		t.Fatal(err)
	}
	secondLock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.count() != 2 {
		t.Fatalf("two adds made %d requests", fetcher.count())
	}
	if firstLock.Assets[at("alpha@1")] != secondLock.Assets[at("alpha@1")] {
		t.Fatal("adding beta changed alpha's lock entry")
	}
	if fetcher.requests[1].URL != "https://example.com/beta" {
		t.Fatalf("second add fetched %#v", fetcher.requests[1])
	}
}

func TestFailedAddDoesNotChangeProjectFiles(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	beforeManifest := projecttest.MustRead(t, manifestPath)
	beforeLock := projecttest.MustRead(t, lockPath)
	fetcher := &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		return nil, errors.New("unavailable")
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)

	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("geo@1"), URL: "https://example.com/one", MaxSize: 100,
	}); err == nil {
		t.Fatal("expected add to fail")
	}
	assertFilesEqual(t, manifestPath, lockPath, beforeManifest, beforeLock)
}

func TestPullUsesCacheWithoutNetwork(t *testing.T) {
	content := []byte("cached")
	manifestPath, lockPath := lockedProject(t, content)
	store := newFakeStore()
	store.objects[digest.Bytes(content)] = bytes.Clone(content)
	fetcher := &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		return nil, errors.New("network must not be used")
	}}
	service := application.New(manifestPath, lockPath, store, fetcher, nil)

	result, err := service.Pull(context.Background(), application.NetworkOptions{Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.count() != 0 || result.Assets[0].Status != "cached" {
		t.Fatalf("unexpected cached pull: requests=%d result=%#v", fetcher.count(), result)
	}
}

func TestPullReportsOfflineMissAndContentMismatch(t *testing.T) {
	content := []byte("expected")
	manifestPath, lockPath := lockedProject(t, content)
	store := newFakeStore()
	fetcher := staticFetcher([]byte("differnt"))
	service := application.New(manifestPath, lockPath, store, fetcher, nil)

	if _, err := service.Pull(context.Background(), application.NetworkOptions{Concurrency: 1, Offline: true}); fault.As(err).Code != "offline_cache_miss" {
		t.Fatalf("expected offline_cache_miss, got %v", err)
	}
	if fetcher.count() != 0 {
		t.Fatal("offline pull made a request")
	}
	if _, err := service.Pull(context.Background(), application.NetworkOptions{Concurrency: 1}); fault.As(err).Code != "content_mismatch" {
		t.Fatalf("expected content_mismatch, got %v", err)
	}
	if fetcher.requests[0].ETag != "" {
		t.Fatalf("pull sent an ETag: %#v", fetcher.requests[0])
	}
}

func TestPullRunsConcurrentlyAndSortsResults(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	manifest := project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("zeta@1"):  {URL: "https://example.com/zeta"},
			at("alpha@1"): {URL: "https://example.com/alpha"},
		},
	}
	lockAssets := make(map[coord.Coordinate]project.LockAsset, len(manifest.Assets))
	for name, asset := range manifest.Assets {
		content := []byte(name.Name)
		lockAssets[name] = project.LockAsset{
			URL:    asset.URL,
			Digest: digest.Bytes(content),
			Size:   int64(len(content)),
		}
	}
	lock, err := project.NewLock(manifest, lockAssets)
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WritePair(manifestPath, lockPath, manifest, lock); err != nil {
		t.Fatal(err)
	}

	var mutex sync.Mutex
	active := 0
	maxActive := 0
	gate := make(chan struct{})
	var once sync.Once
	fetcher := &fakeFetcher{fetch: func(ctx context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
		mutex.Lock()
		active++
		maxActive = max(maxActive, active)
		if active == 2 {
			once.Do(func() { close(gate) })
		}
		mutex.Unlock()
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		mutex.Lock()
		active--
		mutex.Unlock()
		name := strings.TrimPrefix(request.URL, "https://example.com/")
		return response([]byte(name)), nil
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result, err := service.Pull(ctx, application.NetworkOptions{Concurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	if maxActive != 2 {
		t.Fatalf("maximum concurrency was %d", maxActive)
	}
	if result.Assets[0].Name != "alpha" || result.Assets[1].Name != "zeta" {
		t.Fatalf("results are not sorted: %#v", result.Assets)
	}
}

func TestLockRunsConcurrentlyAndSortsResults(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	manifest := project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("zeta@1"):  {URL: "https://example.com/zeta"},
			at("alpha@1"): {URL: "https://example.com/alpha"},
		},
	}
	writeManifest(t, manifestPath, manifest)

	var mutex sync.Mutex
	active := 0
	maxActive := 0
	gate := make(chan struct{})
	var once sync.Once
	fetcher := &fakeFetcher{fetch: func(ctx context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
		mutex.Lock()
		active++
		maxActive = max(maxActive, active)
		if active == 2 {
			once.Do(func() { close(gate) })
		}
		mutex.Unlock()
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		mutex.Lock()
		active--
		mutex.Unlock()
		content := []byte(request.URL)
		return response(content), nil
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	result, err := service.Lock(ctx, application.NetworkOptions{Concurrency: 2, MaxSize: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if maxActive != 2 {
		t.Fatalf("maximum concurrency was %d", maxActive)
	}
	if result.Assets[0].Name != "alpha" || result.Assets[1].Name != "zeta" {
		t.Fatalf("results are not sorted: %#v", result.Assets)
	}
	projecttest.Check(t, manifestPath, lockPath)
}

func TestLockUsesPublisherIntegrityFromCache(t *testing.T) {
	content := []byte("publisher content")
	value := digest.Bytes(content)
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/asset", Integrity: value},
		},
	})
	store := newFakeStore()
	store.objects[value] = bytes.Clone(content)
	fetcher := &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		return nil, errors.New("network must not be used")
	}}
	service := application.New(manifestPath, lockPath, store, fetcher, nil)

	result, err := service.Lock(context.Background(), application.NetworkOptions{Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.count() != 0 || result.Assets[0].Status != "cached" {
		t.Fatalf("unexpected cached lock: requests=%d result=%#v", fetcher.count(), result)
	}
}

func TestRefreshLockUsesStoredETagForRevalidation(t *testing.T) {
	content := []byte("cached")
	manifestPath, lockPath := lockedProject(t, content)
	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	asset := lock.Assets[at("asset@1")]
	asset.ETag = "\"old\""
	lock.Assets[at("asset@1")] = asset
	if err := project.Write(lockPath, lock); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	store.objects[digest.Bytes(content)] = bytes.Clone(content)
	fetcher := &fakeFetcher{fetch: func(_ context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
		if request.ETag != "\"old\"" {
			return nil, fmt.Errorf("ETag is %q", request.ETag)
		}
		return &application.FetchResponse{
			NotModified: true,
			ETag:        "\"new\"",
			Length:      0,
			Body:        io.NopCloser(bytes.NewReader(nil)),
		}, nil
	}}
	service := application.New(manifestPath, lockPath, store, fetcher, nil)

	result, err := service.Lock(context.Background(), application.NetworkOptions{Concurrency: 1, Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Assets[0].Status != "not_modified" {
		t.Fatalf("unexpected lock result: %#v", result)
	}
	updated, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Assets[at("asset@1")].ETag != "\"new\"" {
		t.Fatalf("updated ETag is %q", updated.Assets[at("asset@1")].ETag)
	}
}

func TestNetworkTimeoutHasStableCode(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	fetcher := &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		return nil, application.ErrStalled
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)
	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("asset@1"), URL: "https://example.com/asset",
	}); fault.As(err).Code != "timeout" {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestPathChecksVersionAndCachePresence(t *testing.T) {
	content := []byte("cached")
	manifestPath, lockPath := lockedProject(t, content)
	store := newFakeStore()
	service := application.New(manifestPath, lockPath, store, nil, nil)
	if _, err := service.Path(application.ExactSelection(at("asset@2"))); fault.As(err).Code != "asset_unknown" {
		t.Fatalf("expected asset_unknown, got %v", err)
	}
	if _, err := service.Path(application.ExactSelection(at("asset@1"))); fault.As(err).Code != "cache_object_invalid" {
		t.Fatalf("expected cache_object_invalid, got %v", err)
	}
	store.objects[digest.Bytes(content)] = bytes.Clone(content)
	if _, err := service.Path(application.ExactSelection(at("asset@1"))); err != nil {
		t.Fatal(err)
	}
}

func TestLockHonorsCancellation(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/asset"},
		},
	})
	fetcher := &fakeFetcher{fetch: func(ctx context.Context, _ application.FetchRequest) (*application.FetchResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Lock(ctx, application.NetworkOptions{Concurrency: 1}); fault.As(err).Code != "cancelled" {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestVerifyNeedsNoCacheOrFetcher(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	result, err := application.New(manifestPath, lockPath, nil, nil, nil).Verify(context.Background(), application.NetworkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.AssetCount != 0 || result.ManifestDigest == "" {
		t.Fatalf("unexpected verify result: %#v", result)
	}
}

func staticFetcher(content []byte) *fakeFetcher {
	return &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		return response(content), nil
	}}
}

// sequenceFetcher serves a different body to each request, which is one URL
// whose bytes change: the rolling source a version bump is the honest answer
// to. It repeats the last body once the sequence runs out.
func sequenceFetcher(bodies ...[]byte) *fakeFetcher {
	var served int
	var mutex sync.Mutex
	return &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		mutex.Lock()
		defer mutex.Unlock()
		content := bodies[min(served, len(bodies)-1)]
		served++
		return response(content), nil
	}}
}

// pathFetcher serves the last element of each URL path as its content.
//
// A test about several versions of one asset needs distinct bytes per version,
// because a fetcher answering everything with one body makes every version
// collide, which is a different rule from the one under test. Keying on the
// path rather than the whole URL also lets two hosts serve one file, which is
// what a mirror is.
func pathFetcher() *fakeFetcher {
	return &fakeFetcher{fetch: func(_ context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
		elements := strings.Split(request.URL, "/")
		return response([]byte(elements[len(elements)-1])), nil
	}}
}

func response(content []byte) *application.FetchResponse {
	return &application.FetchResponse{Length: int64(len(content)), Body: io.NopCloser(bytes.NewReader(content))}
}

func emptyProject(t *testing.T) (string, string) {
	t.Helper()
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	manifest, lock := project.Empty()
	if err := project.WritePair(manifestPath, lockPath, manifest, lock); err != nil {
		t.Fatal(err)
	}
	return manifestPath, lockPath
}

func lockedProject(t *testing.T, content []byte) (string, string) {
	t.Helper()
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	manifest := project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/asset"},
		},
	}
	lock, err := project.NewLock(manifest, map[coord.Coordinate]project.LockAsset{
		at("asset@1"): {
			URL:    "https://example.com/asset",
			Digest: digest.Bytes(content),
			Size:   int64(len(content)),
			// The name the URL spells, which is what a lock DAC wrote carries. A
			// fixture without it would be a lock from before file names were
			// recorded, and the tests for that migration write one deliberately.
			Filename: "asset",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WritePair(manifestPath, lockPath, manifest, lock); err != nil {
		t.Fatal(err)
	}
	return manifestPath, lockPath
}

func writeManifest(t *testing.T, path string, manifest project.Manifest) {
	t.Helper()
	data, err := project.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertFilesEqual(t *testing.T, manifestPath, lockPath string, manifest, lock []byte) {
	t.Helper()
	if !bytes.Equal(projecttest.MustRead(t, manifestPath), manifest) || !bytes.Equal(projecttest.MustRead(t, lockPath), lock) {
		t.Fatal("project files changed")
	}
}

// warm installs content into a fake store so a test can start from a cache hit.
func warm(t *testing.T, store *fakeStore, content []byte) {
	t.Helper()
	if _, err := store.Put(context.Background(), bytes.NewReader(content), application.PutAny("", 0)); err != nil {
		t.Fatal(err)
	}
}

// warmStore installs content into a real cache, which Export needs because it
// copies the object out of the filesystem.
func warmStore(t *testing.T, store *cache.Store, content []byte) {
	t.Helper()
	if _, err := store.Put(context.Background(), bytes.NewReader(content), application.PutAny("", 0)); err != nil {
		t.Fatal(err)
	}
}

func failingFetcher(t *testing.T) *fakeFetcher {
	t.Helper()
	return &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		t.Error("the command made a network request")
		return nil, errors.New("unexpected request")
	}}
}

func writeDist(t *testing.T, directory, name string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, name), content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A distribution directory is a cache bundle with the tar taken off: the same
// objects under the same digest names. Import reads both, so the cold-cache
// path for an isolated machine is one command rather than a command and a flag
// on an unrelated one.
func TestImportInstallsFromADistributionDirectory(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	distdir := t.TempDir()
	writeDist(t, distdir, strings.TrimPrefix(digest.Bytes(content), digest.Prefix), content)

	store := newFakeStore()
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)
	result, err := service.Import(context.Background(), distdir)
	if err != nil {
		t.Fatal(err)
	}
	if result.ObjectCount != 1 || result.ByteCount != int64(len(content)) {
		t.Fatalf("unexpected import result: %#v", result)
	}
	if !store.has(application.Object{Digest: digest.Bytes(content), Size: int64(len(content))}) {
		t.Fatal("the object was not installed")
	}
	// The objects are in the cache, so a pull that cannot reach the network
	// finds everything it needs.
	pulled, err := service.Pull(context.Background(), application.NetworkOptions{Concurrency: 1, Offline: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(pulled.Assets) != 1 || pulled.Assets[0].Status != "cached" {
		t.Fatalf("unexpected pull result: %#v", pulled.Assets)
	}
}

// A digest is the only name a consumer can check. DAC used to also accept the
// last element of the asset URL, which meant a directory could satisfy a pull
// with a file whose name proved nothing.
func TestImportSkipsFilesThatAreNotNamedByDigest(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	distdir := t.TempDir()
	writeDist(t, distdir, "asset", content)
	writeDist(t, distdir, "README", []byte("what this share holds"))

	service := application.New(manifestPath, lockPath, newFakeStore(), failingFetcher(t), nil)
	result, err := service.Import(context.Background(), distdir)
	if err != nil {
		t.Fatal(err)
	}
	if result.ObjectCount != 0 || result.ItemCount != 0 {
		t.Fatalf("unexpected import result: %#v", result)
	}
}

func TestImportRefusesAFileThatNamesTheWrongDigest(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	distdir := t.TempDir()
	writeDist(t, distdir, strings.TrimPrefix(digest.Bytes(content), digest.Prefix), []byte("other bytes"))

	service := application.New(manifestPath, lockPath, newFakeStore(), failingFetcher(t), nil)
	_, err := service.Import(context.Background(), distdir)
	if fault.As(err).Code != "import_content_mismatch" {
		t.Fatalf("expected import_content_mismatch, got %v", err)
	}
}

func TestImportAcceptsAnEmptyDirectory(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	service := application.New(manifestPath, lockPath, newFakeStore(), failingFetcher(t), nil)
	result, err := service.Import(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if result.ObjectCount != 0 {
		t.Fatalf("unexpected import result: %#v", result)
	}
}

func TestExportWritesAnIndexedCacheBundle(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	store := cache.New(t.TempDir())
	warmStore(t, store, content)
	service := application.New(manifestPath, lockPath, store, nil, nil)

	bundle := filepath.Join(t.TempDir(), "cache.tar")
	result, err := service.Export(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	if result.Bundle != bundle || result.AssetCount != 1 || result.ByteCount != int64(len(content)) {
		t.Fatalf("unexpected export result: %#v", result)
	}
	file, err := os.Open(bundle)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	reader := tar.NewReader(file)
	header, err := reader.Next()
	if err != nil || header.Name != "index.json" {
		t.Fatalf("unexpected index header: %#v %v", header, err)
	}
	var index struct {
		SchemaVersion int `json:"schemaVersion"`
		Items         []struct {
			Coordinate string `json:"coordinate"`
			SourceURL  string `json:"sourceUrl"`
			File       string `json:"file"`
			Digest     string `json:"digest"`
			Size       int64  `json:"size"`
		} `json:"items"`
	}
	if err := json.NewDecoder(reader).Decode(&index); err != nil {
		t.Fatal(err)
	}
	value := digest.Bytes(content)
	expectedFile := "blobs/sha256/" + strings.TrimPrefix(value, digest.Prefix)
	if index.SchemaVersion != 2 || len(index.Items) != 1 {
		t.Fatalf("unexpected index: %#v", index)
	}
	item := index.Items[0]
	if item.Coordinate != "test/asset@1" || item.SourceURL != "https://example.com/asset" ||
		item.File != expectedFile || item.Digest != value || item.Size != int64(len(content)) {
		t.Fatalf("unexpected bundle item: %#v", item)
	}
	header, err = reader.Next()
	if err != nil || header.Name != expectedFile {
		t.Fatalf("unexpected blob header: %#v %v", header, err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, content) {
		t.Fatalf("exported %q, want %q", data, content)
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected final tar entry: %v", err)
	}
}

func TestImportInstallsAnExportedCacheBundle(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	exportStore := cache.New(t.TempDir())
	warmStore(t, exportStore, content)
	bundle := filepath.Join(t.TempDir(), "cache.tar")
	if _, err := application.New(manifestPath, lockPath, exportStore, nil, nil).Export(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}

	importStore := cache.New(t.TempDir())
	result, err := application.New("", "", importStore, nil, nil).Import(context.Background(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	if result.Bundle != bundle || result.ItemCount != 1 || result.ObjectCount != 1 || result.ByteCount != int64(len(content)) {
		t.Fatalf("unexpected import result: %#v", result)
	}
	object, found, err := importStore.Stat(digest.Bytes(content))
	if err != nil || !found || object.Size != int64(len(content)) {
		t.Fatalf("the import did not install the object: %#v %t %v", object, found, err)
	}
}

func TestImportRejectsBlobContentThatDoesNotMatchTheIndex(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	exportStore := cache.New(t.TempDir())
	warmStore(t, exportStore, content)
	bundle := filepath.Join(t.TempDir(), "cache.tar")
	if _, err := application.New(manifestPath, lockPath, exportStore, nil, nil).Export(context.Background(), bundle); err != nil {
		t.Fatal(err)
	}
	data := projecttest.MustRead(t, bundle)
	position := bytes.Index(data, content)
	if position < 0 {
		t.Fatal("the bundle does not contain the object")
	}
	copy(data[position:position+len(content)], []byte("other bytes"))
	if err := os.WriteFile(bundle, data, 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := application.New("", "", cache.New(t.TempDir()), nil, nil).Import(context.Background(), bundle)
	if fault.As(err).Code != "bundle_invalid" {
		t.Fatalf("expected bundle_invalid, got %v", err)
	}
}

func TestExportRequiresACompleteCache(t *testing.T) {
	manifestPath, lockPath := lockedProject(t, []byte("asset bytes"))
	service := application.New(manifestPath, lockPath, cache.New(t.TempDir()), nil, nil)
	_, err := service.Export(context.Background(), filepath.Join(t.TempDir(), "bundle.tar"))
	if fault.As(err).Code != "cache_object_invalid" {
		t.Fatalf("expected cache_object_invalid, got %v", err)
	}
}

func TestLockTrustsACacheThatSatisfiesIntegrity(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := emptyProject(t)
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/asset", Integrity: digest.Bytes(content)},
		},
	})
	store := newFakeStore()
	warm(t, store, content)
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)
	result, err := service.Lock(context.Background(), application.NetworkOptions{Concurrency: 1, MaxSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	if result.Assets[0].Status != "cached" {
		t.Fatalf("unexpected status %q", result.Assets[0].Status)
	}
}

func TestRefreshLockContactsTheOriginAnyway(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := emptyProject(t)
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/asset", Integrity: digest.Bytes(content)},
		},
	})
	store := newFakeStore()
	warm(t, store, content)
	fetcher := staticFetcher(content)
	service := application.New(manifestPath, lockPath, store, fetcher, nil)
	result, err := service.Lock(context.Background(), application.NetworkOptions{
		Concurrency: 1, MaxSize: 100, Refresh: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.count() != 1 {
		t.Fatalf("refresh made %d requests, want 1", fetcher.count())
	}
	if result.Assets[0].Digest != digest.Bytes(content) {
		t.Fatalf("unexpected digest %q", result.Assets[0].Digest)
	}
}

func TestRefreshLockSendsNoConditionalRequest(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	store := newFakeStore()
	warm(t, store, content)
	fetcher := staticFetcher(content)
	service := application.New(manifestPath, lockPath, store, fetcher, nil)
	if _, err := service.Lock(context.Background(), application.NetworkOptions{
		Concurrency: 1, MaxSize: 100, Refresh: true,
	}); err != nil {
		t.Fatal(err)
	}
	if fetcher.requests[0].ETag != "" {
		t.Fatalf("refresh sent an ETag hint: %q", fetcher.requests[0].ETag)
	}
}

// A pinned asset is settled by its publisher digest, so it never revalidates.
// Recording an ETag for one would store a hint that no later lock could send.
func TestLockRecordsNoETagForAPinnedAsset(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := emptyProject(t)
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/asset", Integrity: digest.Bytes(content)},
		},
	})
	fetcher := &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		served := response(content)
		served.ETag = "\"served\""
		return served, nil
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)

	if _, err := service.Lock(context.Background(), application.NetworkOptions{Concurrency: 1, MaxSize: 100}); err != nil {
		t.Fatal(err)
	}
	locked, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if locked.Assets[at("asset@1")].ETag != "" {
		t.Fatalf("pinned asset recorded ETag %q", locked.Assets[at("asset@1")].ETag)
	}
}

// An ETag written for a pinned asset by an older DAC is dropped rather than
// carried forward, so a repeat lock does not keep rewriting the same field.
func TestLockDropsAStoredETagOnceAnAssetIsPinned(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/asset", Integrity: digest.Bytes(content)},
		},
	})
	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	asset := lock.Assets[at("asset@1")]
	asset.ETag = "\"stale\""
	lock.Assets[at("asset@1")] = asset
	if err := project.Write(lockPath, lock); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	warm(t, store, content)
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)

	result, err := service.Lock(context.Background(), application.NetworkOptions{Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	// The pin matches what the lock already holds, so the entry is carried
	// through without a request. Dropping the ETag is the only edit it needs.
	if result.Assets[0].Status != "locked" {
		t.Fatalf("unexpected status %q", result.Assets[0].Status)
	}
	updated, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Assets[at("asset@1")].ETag != "" {
		t.Fatalf("cached lock kept ETag %q", updated.Assets[at("asset@1")].ETag)
	}
}

func TestAddNormalizesAnSRIIntegrityValue(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := emptyProject(t)
	store := newFakeStore()
	warm(t, store, content)
	service := application.New(manifestPath, lockPath, store, staticFetcher(content), nil)

	sum := sha256.Sum256(content)
	sri := digest.SRIPrefix + base64.StdEncoding.EncodeToString(sum[:])
	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("asset@1"), URL: "https://example.com/asset", Integrity: sri, MaxSize: 100,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := projecttest.Check(t, manifestPath, lockPath)
	if got := manifest.Assets[at("asset@1")].Integrity; got != digest.Bytes(content) {
		t.Fatalf("manifest kept %q, want the canonical form %q", got, digest.Bytes(content))
	}
}

func TestCacheGCReportsWhatItRemoved(t *testing.T) {
	store := newFakeStore()
	warm(t, store, []byte("asset bytes"))
	service := application.New("dac.json", "dac-lock.json", store, nil, nil)
	result, err := service.CacheGC(context.Background(), application.GCOptions{MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if result.ObjectCount != 1 || len(result.Digests) != 1 {
		t.Fatalf("unexpected collection: %#v", result)
	}
}

// The tests below cover what the cache does once an object stops matching the
// digest that names it. That state used to be invisible: every command trusted
// a file that existed at the right path and had the right size.

// seedCorrupt returns a project whose one asset is present in the cache but
// damaged.
func seedCorrupt(t *testing.T, content []byte) (string, string, *fakeStore) {
	t.Helper()
	manifestPath, lockPath := lockedProject(t, content)
	store := newFakeStore()
	value := digest.Bytes(content)
	store.objects[value] = bytes.Clone(content)
	store.damage(value, digest.Bytes([]byte("other bytes")))
	return manifestPath, lockPath, store
}

func TestPathRefusesACorruptObject(t *testing.T) {
	manifestPath, lockPath, store := seedCorrupt(t, []byte("asset bytes"))
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)

	_, err := service.Path(application.ExactSelection(at("asset@1")))
	if code := fault.As(err).Code; code != "cache_object_corrupt" {
		t.Fatalf("expected cache_object_corrupt, got %q (%v)", code, err)
	}
}

func TestInfoReportsACorruptObject(t *testing.T) {
	manifestPath, lockPath, store := seedCorrupt(t, []byte("asset bytes"))
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)

	result, err := service.Info(application.InfoOptions{})
	if err != nil {
		t.Fatalf("info should describe damage rather than fail: %v", err)
	}
	if result.Assets[0].CacheStatus != "corrupt" || result.Summary.CorruptCount != 1 {
		t.Fatalf("unexpected info result: %#v", result)
	}
}

func TestExportRefusesACorruptObject(t *testing.T) {
	manifestPath, lockPath, store := seedCorrupt(t, []byte("asset bytes"))
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)

	_, err := service.Export(context.Background(), t.TempDir())
	if code := fault.As(err).Code; code != "cache_object_corrupt" {
		t.Fatalf("expected cache_object_corrupt, got %q (%v)", code, err)
	}
}

func TestPullRepairsACorruptObject(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath, store := seedCorrupt(t, content)
	service := application.New(manifestPath, lockPath, store, staticFetcher(content), nil)

	result, err := service.Pull(context.Background(), application.NetworkOptions{Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Assets[0].Status != "repaired" {
		t.Fatalf("unexpected status %q", result.Assets[0].Status)
	}
	if store.damaged(digest.Bytes(content)) {
		t.Fatal("the pull did not replace the damaged object")
	}
}

func TestOfflinePullReportsACorruptObjectRatherThanAMiss(t *testing.T) {
	manifestPath, lockPath, store := seedCorrupt(t, []byte("asset bytes"))
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)

	_, err := service.Pull(context.Background(), application.NetworkOptions{Concurrency: 1, Offline: true})
	if code := fault.As(err).Code; code != "cache_object_corrupt" {
		t.Fatalf("expected cache_object_corrupt, got %q (%v)", code, err)
	}
}

func TestVerifyCacheReportsAndRepairs(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath, store := seedCorrupt(t, content)
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)
	value := digest.Bytes(content)

	result, err := service.VerifyCache(context.Background(), application.VerifyCacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Checked != 1 || result.CorruptCount != 1 || result.Repaired != 0 ||
		len(result.Corrupt) != 1 || result.Corrupt[0] != value {
		t.Fatalf("unexpected verify result: %#v", result)
	}
	if !store.damaged(value) {
		t.Fatal("a check without --repair removed the object")
	}

	repaired, err := service.VerifyCache(context.Background(), application.VerifyCacheOptions{Repair: true})
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Repaired != 1 {
		t.Fatalf("unexpected repair result: %#v", repaired)
	}
	if _, found, _ := store.Verify(context.Background(), value); found {
		t.Fatal("--repair left the corrupt object in place")
	}
}

func TestVerifyCacheAllCoversObjectsNoProjectLocked(t *testing.T) {
	manifestPath, lockPath := lockedProject(t, []byte("asset bytes"))
	store := newFakeStore()
	// An object no project references still belongs to the shared cache, so an
	// --all check has to reach it.
	stray := digest.Bytes([]byte("stray"))
	store.objects[stray] = []byte("stray")
	store.damage(stray, digest.Bytes([]byte("junk")))
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)

	scoped, err := service.VerifyCache(context.Background(), application.VerifyCacheOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if scoped.CorruptCount != 0 || scoped.MissingCount != 1 {
		t.Fatalf("a project check should not have seen the stray object: %#v", scoped)
	}

	all, err := service.VerifyCache(context.Background(), application.VerifyCacheOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if all.Checked != 1 || all.CorruptCount != 1 {
		t.Fatalf("unexpected --all result: %#v", all)
	}
}

func TestVerifyRefreshReportsDriftWithoutWriting(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	before := projecttest.MustRead(t, lockPath)
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher([]byte("moved bytes")), nil)

	_, err := service.Verify(context.Background(), application.NetworkOptions{Concurrency: 1, Refresh: true})
	value := fault.As(err)
	if value.Code != "lock_drift" {
		t.Fatalf("expected lock_drift, got %q (%v)", value.Code, err)
	}
	if assets, _ := value.Details["assets"].([]string); len(assets) != 1 || assets[0] != "test/asset@1" {
		t.Fatalf("drift did not name the asset: %#v", value.Details)
	}
	if !bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("--refresh wrote the lock file")
	}
}

func TestVerifyRefreshSucceedsWhenTheOriginsAgree(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(content), nil)

	result, err := service.Verify(context.Background(), application.NetworkOptions{Concurrency: 1, Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Refreshed || result.AssetCount != 1 {
		t.Fatalf("unexpected refresh result: %#v", result)
	}
}

// An ETag is a cache hint a server may rotate over identical bytes. Reporting
// that as drift would page somebody for a header.
func TestVerifyRefreshIgnoresARotatedETag(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	fetcher := &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		served := response(content)
		served.ETag = "\"rotated\""
		return served, nil
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)

	if _, err := service.Verify(context.Background(), application.NetworkOptions{Concurrency: 1, Refresh: true}); err != nil {
		t.Fatal(err)
	}
}

// A manifest edited without locking it is a different failure from an origin
// that moved, and it has a different fix. Verify reports it as the first even
// when it is about to reach the origins.
func TestVerifyRefreshReportsAStaleLockRatherThanDrift(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@2"): {URL: "https://example.com/asset"},
		},
	})
	service := application.New(manifestPath, lockPath, newFakeStore(), failingFetcher(t), nil)

	_, err := service.Verify(context.Background(), application.NetworkOptions{Concurrency: 1, Refresh: true})
	if fault.As(err).Code != "lock_stale" {
		t.Fatalf("expected lock_stale, got %v", err)
	}
}

// The property that lets pull carry the lock command's job without carrying its
// cost. Resolving an asset the lock already describes would mean a conditional
// request for every unpinned asset on every pull, which is what made lock a
// command you thought twice about running.
func TestLockLeavesAgreeingAssetsAlone(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	before := projecttest.MustRead(t, lockPath)
	store := newFakeStore()
	warm(t, store, content)
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)

	result, err := service.Lock(context.Background(), application.NetworkOptions{Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Locked) != 0 {
		t.Fatalf("pull relocked an asset the lock already described: %#v", result.Locked)
	}
	if !bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("pull rewrote a lock file that had nothing to change")
	}
}

// A project whose lock file has never been written is the state a first pull
// exists to leave behind, not an error to report.
func TestLockCreatesAMissingLockFile(t *testing.T) {
	content := []byte("asset bytes")
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/asset"},
		},
	})
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(content), nil)

	result, err := service.Lock(context.Background(), application.NetworkOptions{Concurrency: 1, MaxSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Locked) != 1 || result.Locked[0] != "test/asset@1" {
		t.Fatalf("lock did not report the asset it locked: %#v", result.Locked)
	}
	if !result.Changed {
		t.Fatal("writing a new lock file was not reported as a change")
	}
	// Resolving stored these bytes, so crediting the cache for them would
	// describe a transfer this command had just made as one it avoided.
	if result.Assets[0].Status != "resolved" {
		t.Fatalf("unexpected status %q", result.Assets[0].Status)
	}
	projecttest.Check(t, manifestPath, lockPath)
}

// A pull reproduces the project as committed rather than settling a manifest
// that has moved on. This is the guarantee a deployment job runs on, so a
// manifest the lock no longer describes stops it and names dac lock.
func TestPullRefusesAStaleLock(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/moved"},
		},
	})
	service := application.New(manifestPath, lockPath, newFakeStore(), failingFetcher(t), nil)

	if _, err := service.Pull(context.Background(), application.NetworkOptions{Concurrency: 1}); fault.As(err).Code != "lock_stale" {
		t.Fatalf("expected lock_stale, got %v", err)
	}
}

// Adding an asset to a project that has never been locked writes both files,
// which is what removed the lock command from the path a new project takes.
func TestAddCreatesAMissingLockFile(t *testing.T) {
	content := []byte("asset bytes")
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	writeManifest(t, manifestPath, project.Manifest{SchemaVersion: project.ManifestVersion, Assets: map[coord.Coordinate]project.Asset{}})
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(content), nil)

	result, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("asset@1"), URL: "https://example.com/asset", MaxSize: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Locked) != 0 {
		t.Fatalf("add reported locking assets it did not touch: %#v", result.Locked)
	}
	projecttest.Check(t, manifestPath, lockPath)
}

// An add onto a hand-edited manifest settles the whole project rather than
// writing a lock file that every later command rejects. It says which assets
// that cost, because the operator is about to read the diff.
func TestAddSettlesAHandEditedManifest(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/moved"},
		},
	})
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(content), nil)

	result, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("other@1"), URL: "https://example.com/other", MaxSize: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Locked) != 1 || result.Locked[0] != "test/asset@1" {
		t.Fatalf("add did not report the asset it settled: %#v", result.Locked)
	}
	manifest, lock := projecttest.Check(t, manifestPath, lockPath)
	if manifest.Assets[at("asset@1")].URL != "https://example.com/moved" ||
		lock.Assets[at("asset@1")].URL != "https://example.com/moved" {
		t.Fatalf("add did not settle the edited asset: %#v %#v", manifest.Assets[at("asset@1")], lock.Assets[at("asset@1")])
	}
}

// Removal makes no request, so it settles only what it can offline and names
// what it could not rather than implying the project now agrees.
func TestRemoveReportsWhatItCouldNotLock(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/asset"},
			at("other@1"): {URL: "https://example.com/other"},
			at("gone@1"):  {URL: "https://example.com/gone"},
		},
	})
	service := application.New(manifestPath, lockPath, newFakeStore(), failingFetcher(t), nil)

	result, err := service.Remove(at("gone@1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Unlocked) != 1 || result.Unlocked[0] != "test/other@1" {
		t.Fatalf("remove did not name the unlocked asset: %#v", result.Unlocked)
	}
	manifest, err := project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := manifest.Assets[at("gone@1")]; exists {
		t.Fatal("remove left the asset in the manifest")
	}
}

func TestAddPinRecordsTheResolvedDigest(t *testing.T) {
	content := []byte("asset bytes")
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(content), nil)
	if _, err := service.Init(false); err != nil {
		t.Fatal(err)
	}

	result, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("asset@1"), URL: "https://example.com/asset", Pin: true, MaxSize: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	value := digest.Bytes(content)
	if result.Integrity != value {
		t.Fatalf("add did not pin the asset: %#v", result)
	}
	manifest, lock := projecttest.Check(t, manifestPath, lockPath)
	if manifest.Assets[at("asset@1")].Integrity != value {
		t.Fatalf("the manifest was not pinned: %#v", manifest.Assets[at("asset@1")])
	}
	// A pinned asset never sends a conditional request, so it must not carry an
	// ETag that the next lock would only have to strip back out.
	if lock.Assets[at("asset@1")].ETag != "" {
		t.Fatalf("a pinned lock entry kept an ETag: %#v", lock.Assets[at("asset@1")])
	}
}

func TestAddRejectsPinWithIntegrityOrOffline(t *testing.T) {
	manifestPath, lockPath := lockedProject(t, []byte("asset bytes"))
	service := application.New(manifestPath, lockPath, newFakeStore(), failingFetcher(t), nil)
	for name, options := range map[string]application.AddOptions{
		"integrity": {Coordinate: at("other@1"), URL: "https://example.com/other", Pin: true, Integrity: digest.Bytes([]byte("x"))},
		"offline":   {Coordinate: at("other@1"), URL: "https://example.com/other", Pin: true, Offline: true},
	} {
		if _, err := service.Add(context.Background(), options); fault.As(err).Code != "invalid_arguments" {
			t.Fatalf("%s: expected invalid_arguments, got %v", name, err)
		}
	}
}

// The mistake this whole scheme exists to catch: a new version pointed at the
// source the old one already had. Both versions resolve to one object, so one
// of the two is a label somebody invented for bytes that already had a version.
func TestAddRefusesTwoVersionsOfTheSameBytes(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	before := projecttest.MustRead(t, manifestPath)
	beforeLock := projecttest.MustRead(t, lockPath)
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(content), nil)

	_, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("asset@2"), URL: "https://example.com/asset", MaxSize: 1 << 20,
	})
	value := fault.As(err)
	if value.Code != "version_collision" {
		t.Fatalf("expected version_collision, got %q (%v)", value.Code, err)
	}
	if value.Details["asset"] != "test/asset" {
		t.Fatalf("the collision did not name the asset: %#v", value.Details)
	}
	if versions, _ := value.Details["versions"].([]string); len(versions) != 2 ||
		versions[0] != "1" || versions[1] != "2" {
		t.Fatalf("the collision did not name both versions: %#v", value.Details)
	}
	assertFilesEqual(t, manifestPath, lockPath, before, beforeLock)
}

// The same mistake made by editing the version in place. The old coordinate
// leaves the manifest, so the two never appear together and nothing compares
// them, and the previous lock file is the only thing that still remembers.
func TestAddRefusesAVersionThatNamesRetiredBytes(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@2"): {URL: "https://example.com/asset"},
		},
	})
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(content), nil)

	_, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("other@1"), URL: "https://example.com/other", MaxSize: 1 << 20,
	})
	if code := fault.As(err).Code; code != "version_collision" {
		t.Fatalf("expected version_collision, got %q (%v)", code, err)
	}
}

// Rebinding is the way through when the operator has decided the version does
// mean these bytes now.
func TestAddRebindAcceptsRetiredBytes(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@2"): {URL: "https://example.com/asset"},
		},
	})
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(content), nil)

	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("other@1"), URL: "https://example.com/other",
		AllowRebind: true, MaxSize: 1 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := projecttest.Check(t, manifestPath, lockPath)
	if _, exists := manifest.Assets[at("asset@2")]; !exists {
		t.Fatalf("the rebind did not settle the edited asset: %#v", manifest.Assets)
	}
}

// The rule that gives a version its meaning: once the lock binds a coordinate
// to bytes, nothing rewrites the digest under it. An origin that replaced what
// it serves behind a stable URL is the case this exists for, and a refresh is
// where DAC used to accept it silently.
func TestRefreshLockRefusesToRebindALockedVersion(t *testing.T) {
	manifestPath, lockPath := lockedProject(t, []byte("asset bytes"))
	before := projecttest.MustRead(t, lockPath)
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher([]byte("moved bytes")), nil)

	_, err := service.Lock(context.Background(), application.NetworkOptions{
		Concurrency: 1, MaxSize: 1 << 20, Refresh: true,
	})
	value := fault.As(err)
	if value.Code != "version_rebind" {
		t.Fatalf("expected version_rebind, got %q (%v)", value.Code, err)
	}
	if value.Details["asset"] != "test/asset@1" {
		t.Fatalf("the rebind did not name the asset: %#v", value.Details)
	}
	if value.Details["lockedDigest"] != digest.Bytes([]byte("asset bytes")) ||
		value.Details["resolvedDigest"] != digest.Bytes([]byte("moved bytes")) {
		t.Fatalf("the rebind did not report both digests: %#v", value.Details)
	}
	if !bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("the refused rebind still wrote the lock file")
	}
}

// A manifest edited to point one version somewhere else is the same claim from
// the other direction, so it fails the same way.
func TestLockRefusesToRebindThroughAManifestEdit(t *testing.T) {
	manifestPath, lockPath := lockedProject(t, []byte("asset bytes"))
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/elsewhere"},
		},
	})
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher([]byte("other bytes")), nil)

	_, err := service.Lock(context.Background(), application.NetworkOptions{
		Concurrency: 1, MaxSize: 1 << 20,
	})
	if code := fault.As(err).Code; code != "version_rebind" {
		t.Fatalf("expected version_rebind, got %q (%v)", code, err)
	}
}

// A source that moved but still serves the same bytes is not a rebind. Nothing
// about what the version names has changed, so nothing needs deciding.
func TestLockAcceptsAMovedSourceServingTheSameBytes(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://mirror.example.com/asset"},
		},
	})
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(content), nil)

	if _, err := service.Lock(context.Background(), application.NetworkOptions{
		Concurrency: 1, MaxSize: 1 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Assets[at("asset@1")].URL != "https://mirror.example.com/asset" {
		t.Fatalf("the lock did not follow the source: %#v", lock.Assets[at("asset@1")])
	}
}

// Rebinding stays available for the project that tracks a rolling source and
// has decided to accept what it now serves.
func TestRebindWritesTheNewBytes(t *testing.T) {
	manifestPath, lockPath := lockedProject(t, []byte("asset bytes"))
	moved := []byte("moved bytes")
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(moved), nil)

	if _, err := service.Lock(context.Background(), application.NetworkOptions{
		Concurrency: 1, MaxSize: 1 << 20, Refresh: true, AllowRebind: true,
	}); err != nil {
		t.Fatal(err)
	}
	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Assets[at("asset@1")].Digest != digest.Bytes(moved) {
		t.Fatalf("the rebind did not write the new digest: %#v", lock.Assets[at("asset@1")])
	}
}

// Verify reports drift as its result, so the rebind rule must not raise a
// different error in front of the answer this command exists to give.
func TestVerifyRefreshStillReportsDriftRatherThanRebind(t *testing.T) {
	manifestPath, lockPath := lockedProject(t, []byte("asset bytes"))
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher([]byte("moved bytes")), nil)

	_, err := service.Verify(context.Background(), application.NetworkOptions{Concurrency: 1, MaxSize: 1 << 20, Refresh: true})
	if code := fault.As(err).Code; code != "lock_drift" {
		t.Fatalf("expected lock_drift, got %q (%v)", code, err)
	}
}

// Two versions of one asset served from one URL is fragile rather than wrong:
// only one set of bytes is at that URL, so a cold cache can restore only one of
// them. The add says so and continues.
func TestAddReportsASharedSourceURL(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	// A genuinely rolling source: one URL whose bytes change between the two
	// adds. That is the only way two versions can share a URL without also
	// naming the same object, which is the rule that would answer first.
	service := application.New(manifestPath, lockPath, newFakeStore(),
		sequenceFetcher([]byte("first bytes"), []byte("later bytes")), nil)

	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("geo@1"), URL: "https://example.com/rolling", MaxSize: 1 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("geo@2"), URL: "https://example.com/rolling", MaxSize: 1 << 20,
	})
	if err != nil {
		t.Fatalf("a shared source URL is fragile, not an error: %v", err)
	}
	if len(result.SharedSources) != 1 || result.SharedSources[0] != "1" {
		t.Fatalf("add did not report the shared source: %#v", result.SharedSources)
	}
	if len(result.Siblings) != 1 || result.Siblings[0] != "1" {
		t.Fatalf("add did not report the sibling version: %#v", result.Siblings)
	}
}

// namedFetcher serves one body under a name the origin supplies, which is what
// a Content-Disposition header amounts to by the time it reaches the service.
func namedFetcher(content []byte, name string) *fakeFetcher {
	return &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		value := response(content)
		value.Filename = name
		return value, nil
	}}
}

// unlockedProject writes a lock entry with no file name: a project locked by a
// DAC from before the field existed. It is the starting state for every
// migration test here.
func unlockedNameProject(t *testing.T, content []byte) (string, string) {
	t.Helper()
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	manifest := project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/geo/database.bin"},
		},
	}
	lock, err := project.NewLock(manifest, map[coord.Coordinate]project.LockAsset{
		at("asset@1"): {
			URL:    "https://example.com/geo/database.bin",
			Digest: digest.Bytes(content),
			Size:   int64(len(content)),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WritePair(manifestPath, lockPath, manifest, lock); err != nil {
		t.Fatal(err)
	}
	return manifestPath, lockPath
}

func lockedFilename(t *testing.T, lockPath string) string {
	t.Helper()
	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	return lock.Assets[at("asset@1")].Filename
}

// The name an origin supplies is the whole point of the field: a URL ending in
// an opaque endpoint spells nothing useful, and the header is the only place
// the real name appears.
func TestResolveRecordsTheNameTheOriginSupplies(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := emptyProject(t)
	service := application.New(manifestPath, lockPath, newFakeStore(),
		namedFetcher(content, "database.bin"), nil)

	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("asset@1"), URL: "https://example.com/download?id=1234", MaxSize: 1 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	if name := lockedFilename(t, lockPath); name != "database.bin" {
		t.Fatalf("lock recorded %q, want the supplied name", name)
	}
}

// Without a supplied name the URL is the only thing left that spells one.
func TestResolveFallsBackToTheNameTheURLSpells(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := emptyProject(t)
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(content), nil)

	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("asset@1"), URL: "https://example.com/geo/database.bin", MaxSize: 1 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	if name := lockedFilename(t, lockPath); name != "database.bin" {
		t.Fatalf("lock recorded %q, want the name the URL spells", name)
	}
}

// A name an origin supplies that is not one path element is refused rather than
// repaired, and the URL answers instead. Nothing about the command fails: the
// field decides nothing, so a hostile header costs a worse name and no more.
func TestResolveRefusesASuppliedNameThatEscapesItsDirectory(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := emptyProject(t)
	service := application.New(manifestPath, lockPath, newFakeStore(),
		namedFetcher(content, "../../etc/passwd"), nil)

	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("asset@1"), URL: "https://example.com/geo/database.bin", MaxSize: 1 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	if name := lockedFilename(t, lockPath); name != "database.bin" {
		t.Fatalf("lock recorded %q, want the URL name instead of the escaping one", name)
	}
}

// A lock from before the field existed gains a name without a request, because
// the URL already holds the answer. An asset that agrees with its manifest is
// never resolved again, so a migration that waited for a resolution would never
// reach a project that had settled.
func TestLockBackfillsAMissingFilenameWithoutARequest(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := unlockedNameProject(t, content)
	if name := lockedFilename(t, lockPath); name != "" {
		t.Fatalf("the fixture already carries the name %q", name)
	}
	store := newFakeStore()
	warm(t, store, content)
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)

	if _, err := service.Lock(context.Background(), application.NetworkOptions{
		Concurrency: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if name := lockedFilename(t, lockPath); name != "database.bin" {
		t.Fatalf("the backfilled name is %q", name)
	}

	// The rewrite is the migration and happens once. A pull that kept rewriting
	// the lock would have anything watching the project directory read a change
	// on every run.
	before := projecttest.MustRead(t, lockPath)
	if _, err := service.Lock(context.Background(), application.NetworkOptions{
		Concurrency: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("a second pull rewrote the lock file again")
	}
}

// A missing name never makes a lock look stale. Every project locked before
// this field existed would otherwise have to re-resolve every asset it holds.
func TestALockWithNoFilenameStillDescribesItsManifest(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := unlockedNameProject(t, content)
	store := newFakeStore()
	warm(t, store, content)
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)

	result, err := service.Pull(context.Background(), application.NetworkOptions{Concurrency: 1})
	if err != nil {
		t.Fatalf("a lock with no file name was rejected: %v", err)
	}
	if len(result.Assets) != 1 || result.Assets[0].Cached != true {
		t.Fatalf("unexpected pull result: %#v", result.Assets)
	}
	// A pull writes no project files at all, so the name is still absent.
	if name := lockedFilename(t, lockPath); name != "" {
		t.Fatalf("a plain pull wrote %q to the lock file", name)
	}
}

// A manifest that repoints an asset replaces the source the old name described,
// so the name goes with it rather than following the bytes.
func TestResolveDropsTheOldNameWhenTheURLMoves(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	service := application.New(manifestPath, lockPath, newFakeStore(), pathFetcher(), nil)

	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("asset@1"), URL: "https://example.com/old/first.bin", MaxSize: 1 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	if name := lockedFilename(t, lockPath); name != "first.bin" {
		t.Fatalf("lock recorded %q", name)
	}

	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/new/second.bin"},
		},
	})
	if _, err := service.Lock(context.Background(), application.NetworkOptions{
		Concurrency: 1, AllowRebind: true,
	}); err != nil {
		t.Fatal(err)
	}
	if name := lockedFilename(t, lockPath); name != "second.bin" {
		t.Fatalf("lock kept %q after the URL moved", name)
	}
}

// The asset view is where a script reads the name, so it has to carry what the
// lock holds.
func TestInfoReportsTheLockedFilename(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	store := newFakeStore()
	warm(t, store, content)
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)

	result, err := service.Info(application.InfoOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Assets) != 1 || result.Assets[0].Filename != "asset" {
		t.Fatalf("info did not report the locked file name: %#v", result.Assets)
	}
}

// A 304 says these are the bytes the lock already describes, so it must not
// rename them. The response carries no body and rarely repeats the header that
// named the asset, so honoring whatever it reports would trade a name the origin
// once gave for whatever the URL happens to spell.
func TestNotModifiedKeepsTheRecordedFilename(t *testing.T) {
	content := []byte("cached")
	manifestPath, lockPath := lockedProject(t, content)
	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	asset := lock.Assets[at("asset@1")]
	asset.ETag = "\"old\""
	// A name only a Content-Disposition header could have produced: the URL of
	// this fixture spells "asset".
	asset.Filename = "database.bin"
	lock.Assets[at("asset@1")] = asset
	if err := project.Write(lockPath, lock); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	warm(t, store, content)
	fetcher := &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		return &application.FetchResponse{
			NotModified: true,
			ETag:        "\"new\"",
			// What the adapter reports for a 304 with no header: the name the
			// URL spells, which is worse than the one already recorded.
			Filename: "asset",
			Body:     io.NopCloser(bytes.NewReader(nil)),
		}, nil
	}}
	service := application.New(manifestPath, lockPath, store, fetcher, nil)

	result, err := service.Lock(context.Background(), application.NetworkOptions{
		Concurrency: 1, Refresh: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Assets[0].Status != "not_modified" {
		t.Fatalf("unexpected pull result: %#v", result.Assets)
	}
	if name := lockedFilename(t, lockPath); name != "database.bin" {
		t.Fatalf("a not-modified response changed the recorded name to %q", name)
	}
}

// The same response does fill in an entry that has no name, which is how a lock
// from before the field existed migrates through a refresh.
func TestNotModifiedBackfillsAnAbsentFilename(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := unlockedNameProject(t, content)
	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	asset := lock.Assets[at("asset@1")]
	asset.ETag = "\"old\""
	lock.Assets[at("asset@1")] = asset
	if err := project.Write(lockPath, lock); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	warm(t, store, content)
	fetcher := &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		return &application.FetchResponse{
			NotModified: true,
			ETag:        "\"new\"",
			Filename:    "database.bin",
			Body:        io.NopCloser(bytes.NewReader(nil)),
		}, nil
	}}
	service := application.New(manifestPath, lockPath, store, fetcher, nil)

	if _, err := service.Lock(context.Background(), application.NetworkOptions{
		Concurrency: 1, Refresh: true,
	}); err != nil {
		t.Fatal(err)
	}
	if name := lockedFilename(t, lockPath); name != "database.bin" {
		t.Fatalf("the backfilled name is %q", name)
	}
}
