package application_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
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

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/cache"
	"github.com/tom/dac/internal/digest"
	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/project"
	"github.com/tom/dac/internal/projecttest"
	"github.com/tom/dac/internal/rewrite"
)

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
		Name: "geo", Version: "1", URL: "https://example.com/one", MaxSize: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if added.Name != "geo" || added.Version != "1" || !added.Cached {
		t.Fatalf("unexpected add result: %#v", added)
	}
	projecttest.Check(t, manifestPath, lockPath)
	if fetcher.count() != 1 {
		t.Fatalf("add made %d requests", fetcher.count())
	}

	beforeManifest := projecttest.MustRead(t, manifestPath)
	beforeLock := projecttest.MustRead(t, lockPath)
	if _, err := service.Add(context.Background(), application.AddOptions{
		Name: "geo", Version: "2", URL: "https://example.com/two", MaxSize: 100,
	}); fault.As(err).Code != "asset_exists" {
		t.Fatalf("expected asset_exists, got %v", err)
	}
	assertFilesEqual(t, manifestPath, lockPath, beforeManifest, beforeLock)

	if _, err := service.Remove("geo", "2"); fault.As(err).Code != "asset_unknown" {
		t.Fatalf("expected asset_unknown, got %v", err)
	}
	if _, err := service.Remove("geo", "1"); err != nil {
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
	result, err := service.Info(application.InfoOptions{Name: "asset", Version: "1", Rewriter: config})
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
	if _, err := service.Info(application.InfoOptions{Name: "asset", Version: "2", Rewriter: config}); fault.As(err).Code != "asset_unknown" {
		t.Fatalf("expected asset_unknown, got %v", err)
	}
}

func TestAddForceReplacesOneVersion(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	store := newFakeStore()
	fetcher := staticFetcher([]byte("content"))
	service := application.New(manifestPath, lockPath, store, fetcher, nil)
	if _, err := service.Add(context.Background(), application.AddOptions{
		Name: "geo", Version: "1", URL: "https://example.com/one", MaxSize: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Add(context.Background(), application.AddOptions{
		Name: "geo", Version: "2", URL: "https://example.com/two", Force: true, MaxSize: 100,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, lock := projecttest.Check(t, manifestPath, lockPath)
	if manifest.Assets["geo"].Version != "2" || lock.Assets["geo"].Version != "2" {
		t.Fatalf("force did not replace the version: %#v %#v", manifest.Assets["geo"], lock.Assets["geo"])
	}
	if len(manifest.Assets) != 1 || len(lock.Assets) != 1 {
		t.Fatal("force created more than one active version")
	}
}

func TestAddResolvesOnlyTheChangedAsset(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	fetcher := staticFetcher([]byte("content"))
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)
	if _, err := service.Add(context.Background(), application.AddOptions{
		Name: "alpha", Version: "1", URL: "https://example.com/alpha", MaxSize: 100,
	}); err != nil {
		t.Fatal(err)
	}
	firstLock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Add(context.Background(), application.AddOptions{
		Name: "beta", Version: "1", URL: "https://example.com/beta", MaxSize: 100,
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
	if firstLock.Assets["alpha"] != secondLock.Assets["alpha"] {
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
		Name: "geo", Version: "1", URL: "https://example.com/one", MaxSize: 100,
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
		Assets: map[string]project.Asset{
			"zeta":  {Version: "1", URL: "https://example.com/zeta"},
			"alpha": {Version: "1", URL: "https://example.com/alpha"},
		},
	}
	lockAssets := make(map[string]project.LockAsset, len(manifest.Assets))
	for name, asset := range manifest.Assets {
		content := []byte(name)
		lockAssets[name] = project.LockAsset{
			Version: asset.Version,
			URL:     asset.URL,
			Digest:  digest.Bytes(content),
			Size:    int64(len(content)),
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
		Assets: map[string]project.Asset{
			"zeta":  {Version: "1", URL: "https://example.com/zeta"},
			"alpha": {Version: "1", URL: "https://example.com/alpha"},
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
		Assets: map[string]project.Asset{
			"asset": {Version: "1", URL: "https://example.com/asset", Integrity: value},
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

func TestLockUsesStoredETagForRevalidation(t *testing.T) {
	content := []byte("cached")
	manifestPath, lockPath := lockedProject(t, content)
	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	asset := lock.Assets["asset"]
	asset.ETag = "\"old\""
	lock.Assets["asset"] = asset
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

	result, err := service.Lock(context.Background(), application.NetworkOptions{Concurrency: 1})
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
	if updated.Assets["asset"].ETag != "\"new\"" {
		t.Fatalf("updated ETag is %q", updated.Assets["asset"].ETag)
	}
}

func TestNetworkTimeoutHasStableCode(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	fetcher := &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		return nil, application.ErrStalled
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)
	if _, err := service.Add(context.Background(), application.AddOptions{
		Name: "asset", Version: "1", URL: "https://example.com/asset",
	}); fault.As(err).Code != "timeout" {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func TestPathChecksVersionAndCachePresence(t *testing.T) {
	content := []byte("cached")
	manifestPath, lockPath := lockedProject(t, content)
	store := newFakeStore()
	service := application.New(manifestPath, lockPath, store, nil, nil)
	if _, err := service.Path("asset", "2"); fault.As(err).Code != "asset_unknown" {
		t.Fatalf("expected asset_unknown, got %v", err)
	}
	if _, err := service.Path("asset", "1"); fault.As(err).Code != "cache_object_invalid" {
		t.Fatalf("expected cache_object_invalid, got %v", err)
	}
	store.objects[digest.Bytes(content)] = bytes.Clone(content)
	if _, err := service.Path("asset", "1"); err != nil {
		t.Fatal(err)
	}
}

func TestLockHonorsCancellation(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[string]project.Asset{
			"asset": {Version: "1", URL: "https://example.com/asset"},
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
	result, err := application.New(manifestPath, lockPath, nil, nil, nil).Verify()
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
		Assets: map[string]project.Asset{
			"asset": {Version: "1", URL: "https://example.com/asset"},
		},
	}
	lock, err := project.NewLock(manifest, map[string]project.LockAsset{
		"asset": {
			Version: "1",
			URL:     "https://example.com/asset",
			Digest:  digest.Bytes(content),
			Size:    int64(len(content)),
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

func TestPullInstallsFromADistributionDirectory(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	distdir := t.TempDir()
	writeDist(t, distdir, strings.TrimPrefix(digest.Bytes(content), digest.Prefix), content)

	store := newFakeStore()
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)
	result, err := service.Pull(context.Background(), application.NetworkOptions{
		Concurrency: 1, Offline: true, DistDir: distdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Assets) != 1 || result.Assets[0].Status != "distdir" {
		t.Fatalf("unexpected pull result: %#v", result.Assets)
	}
	if !store.has(application.Object{Digest: digest.Bytes(content), Size: int64(len(content))}) {
		t.Fatal("the object was not installed")
	}
}

// TestPullIgnoresADistributionFileNamedAfterTheURL fixes the naming rule at one
// option. DAC used to also accept the last element of the asset URL, which
// meant a bundle could satisfy a pull with a file whose name proved nothing.
func TestPullIgnoresADistributionFileNamedAfterTheURL(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	distdir := t.TempDir()
	writeDist(t, distdir, "asset", content)

	service := application.New(manifestPath, lockPath, newFakeStore(), failingFetcher(t), nil)
	_, err := service.Pull(context.Background(), application.NetworkOptions{
		Concurrency: 1, Offline: true, DistDir: distdir,
	})
	if fault.As(err).Code != "offline_cache_miss" {
		t.Fatalf("expected an offline cache miss, got %v", err)
	}
}

func TestPullRefusesADistributionFileThatNamesTheWrongDigest(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	distdir := t.TempDir()
	writeDist(t, distdir, strings.TrimPrefix(digest.Bytes(content), digest.Prefix), []byte("other bytes"))

	service := application.New(manifestPath, lockPath, newFakeStore(), failingFetcher(t), nil)
	_, err := service.Pull(context.Background(), application.NetworkOptions{
		Concurrency: 1, Offline: true, DistDir: distdir,
	})
	if fault.As(err).Code != "content_mismatch" {
		t.Fatalf("expected content_mismatch, got %v", err)
	}
}

func TestPullFallsPastAURLNamedFileThatDoesNotMatch(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	distdir := t.TempDir()
	writeDist(t, distdir, "asset", []byte("a different asset that happens to share a name"))

	service := application.New(manifestPath, lockPath, newFakeStore(), failingFetcher(t), nil)
	_, err := service.Pull(context.Background(), application.NetworkOptions{
		Concurrency: 1, Offline: true, DistDir: distdir,
	})
	if fault.As(err).Code != "offline_cache_miss" {
		t.Fatalf("expected offline_cache_miss, got %v", err)
	}
}

func TestPullIgnoresAnEmptyDistributionDirectory(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	store := newFakeStore()
	fetcher := staticFetcher(content)
	service := application.New(manifestPath, lockPath, store, fetcher, nil)
	result, err := service.Pull(context.Background(), application.NetworkOptions{
		Concurrency: 1, DistDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Assets[0].Status != "downloaded" || fetcher.count() != 1 {
		t.Fatalf("unexpected result %#v after %d requests", result.Assets[0], fetcher.count())
	}
}

func TestExportWritesObjectsNamedByDigest(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	store := cache.New(t.TempDir())
	warmStore(t, store, content)
	service := application.New(manifestPath, lockPath, store, nil, nil)

	directory := filepath.Join(t.TempDir(), "bundle")
	result, err := service.Export(context.Background(), directory)
	if err != nil {
		t.Fatal(err)
	}
	if result.AssetCount != 1 || result.ByteCount != int64(len(content)) {
		t.Fatalf("unexpected export result: %#v", result)
	}
	path := filepath.Join(directory, strings.TrimPrefix(digest.Bytes(content), digest.Prefix))
	if data := projecttest.MustRead(t, path); !bytes.Equal(data, content) {
		t.Fatalf("exported %q, want %q", data, content)
	}
}

func TestExportRequiresACompleteCache(t *testing.T) {
	manifestPath, lockPath := lockedProject(t, []byte("asset bytes"))
	service := application.New(manifestPath, lockPath, cache.New(t.TempDir()), nil, nil)
	_, err := service.Export(context.Background(), filepath.Join(t.TempDir(), "bundle"))
	if fault.As(err).Code != "cache_object_invalid" {
		t.Fatalf("expected cache_object_invalid, got %v", err)
	}
}

func TestLockTrustsACacheThatSatisfiesIntegrity(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := emptyProject(t)
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[string]project.Asset{
			"asset": {Version: "1", URL: "https://example.com/asset", Integrity: digest.Bytes(content)},
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

func TestLockRefreshContactsTheOriginAnyway(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := emptyProject(t)
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[string]project.Asset{
			"asset": {Version: "1", URL: "https://example.com/asset", Integrity: digest.Bytes(content)},
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

func TestLockRefreshSendsNoConditionalRequest(t *testing.T) {
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
		Assets: map[string]project.Asset{
			"asset": {Version: "1", URL: "https://example.com/asset", Integrity: digest.Bytes(content)},
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
	if locked.Assets["asset"].ETag != "" {
		t.Fatalf("pinned asset recorded ETag %q", locked.Assets["asset"].ETag)
	}
}

// An ETag written for a pinned asset by an older DAC is dropped rather than
// carried forward, so a repeat lock does not keep rewriting the same field.
func TestLockDropsAStoredETagOnceAnAssetIsPinned(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[string]project.Asset{
			"asset": {Version: "1", URL: "https://example.com/asset", Integrity: digest.Bytes(content)},
		},
	})
	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	asset := lock.Assets["asset"]
	asset.ETag = "\"stale\""
	lock.Assets["asset"] = asset
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
	if result.Assets[0].Status != "cached" {
		t.Fatalf("unexpected status %q", result.Assets[0].Status)
	}
	updated, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Assets["asset"].ETag != "" {
		t.Fatalf("cached lock kept ETag %q", updated.Assets["asset"].ETag)
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
		Name: "asset", Version: "1", URL: "https://example.com/asset", Integrity: sri, MaxSize: 100,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, _ := projecttest.Check(t, manifestPath, lockPath)
	if got := manifest.Assets["asset"].Integrity; got != digest.Bytes(content) {
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

	_, err := service.Path("asset", "1")
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

func TestLockCheckReportsDriftWithoutWriting(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	before := projecttest.MustRead(t, lockPath)
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher([]byte("moved bytes")), nil)

	_, err := service.Lock(context.Background(), application.NetworkOptions{Concurrency: 1, Check: true})
	value := fault.As(err)
	if value.Code != "lock_drift" {
		t.Fatalf("expected lock_drift, got %q (%v)", value.Code, err)
	}
	if assets, _ := value.Details["assets"].([]string); len(assets) != 1 || assets[0] != "asset" {
		t.Fatalf("drift did not name the asset: %#v", value.Details)
	}
	if !bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("--check wrote the lock file")
	}
}

func TestLockCheckSucceedsWhenTheLockIsCurrent(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(content), nil)

	result, err := service.Lock(context.Background(), application.NetworkOptions{Concurrency: 1, Check: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Changed || len(result.Drifted) != 0 {
		t.Fatalf("unexpected check result: %#v", result)
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
		Name: "asset", Version: "1", URL: "https://example.com/asset", Pin: true, MaxSize: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	value := digest.Bytes(content)
	if result.Integrity != value {
		t.Fatalf("add did not pin the asset: %#v", result)
	}
	manifest, lock := projecttest.Check(t, manifestPath, lockPath)
	if manifest.Assets["asset"].Integrity != value {
		t.Fatalf("the manifest was not pinned: %#v", manifest.Assets["asset"])
	}
	// A pinned asset never sends a conditional request, so it must not carry an
	// ETag that the next lock would only have to strip back out.
	if lock.Assets["asset"].ETag != "" {
		t.Fatalf("a pinned lock entry kept an ETag: %#v", lock.Assets["asset"])
	}
}

func TestAddRejectsPinWithIntegrityOrOffline(t *testing.T) {
	manifestPath, lockPath := lockedProject(t, []byte("asset bytes"))
	service := application.New(manifestPath, lockPath, newFakeStore(), failingFetcher(t), nil)
	for name, options := range map[string]application.AddOptions{
		"integrity": {Name: "other", Version: "1", URL: "https://example.com/other", Pin: true, Integrity: digest.Bytes([]byte("x"))},
		"offline":   {Name: "other", Version: "1", URL: "https://example.com/other", Pin: true, Offline: true},
	} {
		if _, err := service.Add(context.Background(), options); fault.As(err).Code != "invalid_arguments" {
			t.Fatalf("%s: expected invalid_arguments, got %v", name, err)
		}
	}
}

func TestAddForceReportsTheRetiredCoordinate(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(content), nil)

	result, err := service.Add(context.Background(), application.AddOptions{
		Name: "asset", Version: "2", URL: "https://example.com/asset", Force: true, MaxSize: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Replaced != "asset@1" {
		t.Fatalf("add did not report the coordinate it retired: %#v", result)
	}
}
