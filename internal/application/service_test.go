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

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/dac/internal/projecttest"
)

// at builds a test coordinate from its name and version.
func at(text string) coord.Coordinate { return coord.MustParse("test/" + text) }

var (
	_ application.ObjectStore = (*fakeStore)(nil)
	_ application.Fetcher     = (*fakeFetcher)(nil)
	_ application.Reporter    = (*fakeReporter)(nil)
)

type fakeStore struct {
	mutex   sync.Mutex
	objects map[string][]byte
	// corrupt maps a digest to the digest its bytes actually have, so a test can reproduce a damaged cache without going near a filesystem.
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

// inspect reports the object a digest names, and the corruption recorded for it.
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

// Describe reports an object without touching its liveness timestamp.
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

func (reporter *fakeReporter) Plan(names []string) {
	reporter.add("plan:" + strings.Join(names, ","))
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

func TestAddAndRemoveChangeOnlyTheManifest(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	store := newFakeStore()
	fetcher := staticFetcher([]byte("one"))
	reporter := &fakeReporter{}
	service := application.New(manifestPath, lockPath, store, fetcher, reporter)
	lockBefore := projecttest.MustRead(t, lockPath)

	added, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("geo@1"), URL: "https://example.com/one", MaxSize: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if added.Coordinate != "test/geo@1" || added.Namespace != "test" || added.Name != "geo" ||
		added.Version != "1" || added.Cached || added.Status != "unlocked" {
		t.Fatalf("unexpected add result: %#v", added)
	}
	if !bytes.Equal(lockBefore, projecttest.MustRead(t, lockPath)) {
		t.Fatal("add changed the lock file")
	}
	if fetcher.count() != 0 {
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
	manifest, err := project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Assets) != 0 {
		t.Fatalf("remove left assets: %#v", manifest.Assets)
	}
	if !bytes.Equal(lockBefore, projecttest.MustRead(t, lockPath)) {
		t.Fatal("remove changed the lock file")
	}
	if fetcher.count() != 0 {
		t.Fatal("remove made a network request")
	}
	if len(reporter.events) == 0 || reporter.events[len(reporter.events)-1] != "wait" {
		t.Fatalf("reporter did not receive its final wait: %v", reporter.events)
	}
}

// A version is part of an asset's identity, so a second version is a second entry.
func TestAddKeepsBothVersionsOfAnAsset(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	lockBefore := projecttest.MustRead(t, lockPath)
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
	manifest, err := project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Assets) != 2 {
		t.Fatalf("the manifest does not hold both versions: %#v", manifest.Assets)
	}
	if manifest.Assets[at("geo@1")].URL != "https://example.com/one" ||
		manifest.Assets[at("geo@2")].URL != "https://example.com/two" {
		t.Fatalf("the versions do not have their own sources: %#v", manifest.Assets)
	}
	if !bytes.Equal(lockBefore, projecttest.MustRead(t, lockPath)) {
		t.Fatal("adds changed the lock file")
	}
}

// Force is now only about one coordinate: it replaces the source of a version the manifest already has, and leaves every other version alone.
func TestAddForceReplacesOneVersionSource(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	lockBefore := projecttest.MustRead(t, lockPath)
	service := application.New(manifestPath, lockPath, newFakeStore(), pathFetcher(), nil)
	for _, options := range []application.AddOptions{
		{Coordinate: at("geo@1"), URL: "https://example.com/one", MaxSize: 100},
		{Coordinate: at("geo@2"), URL: "https://example.com/two", MaxSize: 100},
	} {
		if _, err := service.Add(context.Background(), options); err != nil {
			t.Fatal(err)
		}
	}
	// A moved source that serves the same bytes is not a rebind, so replacing one version's URL needs nothing beyond --force.
	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("geo@2"), URL: "https://mirror.example.com/two", Force: true, MaxSize: 100,
	}); err != nil {
		t.Fatal(err)
	}
	manifest, err := project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Assets) != 2 {
		t.Fatalf("force changed the number of versions: %#v", manifest.Assets)
	}
	if manifest.Assets[at("geo@1")].URL != "https://example.com/one" ||
		manifest.Assets[at("geo@2")].URL != "https://mirror.example.com/two" {
		t.Fatalf("force replaced the wrong version: %#v", manifest.Assets)
	}
	if !bytes.Equal(lockBefore, projecttest.MustRead(t, lockPath)) {
		t.Fatal("force changed the lock file")
	}
}

func TestAddResolvesNothingWithoutPin(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	fetcher := staticFetcher([]byte("content"))
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)
	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("alpha@1"), URL: "https://example.com/alpha", MaxSize: 100,
	}); err != nil {
		t.Fatal(err)
	}
	firstLock := projecttest.MustRead(t, lockPath)
	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("beta@1"), URL: "https://example.com/beta", MaxSize: 100,
	}); err != nil {
		t.Fatal(err)
	}
	secondLock := projecttest.MustRead(t, lockPath)
	if fetcher.count() != 0 {
		t.Fatalf("two adds made %d requests", fetcher.count())
	}
	if !bytes.Equal(firstLock, secondLock) {
		t.Fatal("adding beta changed the lock file")
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
		Coordinate: at("geo@1"), URL: "https://example.com/one", Pin: true, MaxSize: 100,
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

	result, err := service.Pull(context.Background(), application.PullOptions{Concurrency: 2})
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

	if _, err := service.Pull(context.Background(), application.PullOptions{Concurrency: 1, Offline: true}); fault.As(err).Code != "offline_cache_miss" {
		t.Fatalf("expected offline_cache_miss, got %v", err)
	}
	if fetcher.count() != 0 {
		t.Fatal("offline pull made a request")
	}
	if _, err := service.Pull(context.Background(), application.PullOptions{Concurrency: 1}); fault.As(err).Code != "content_mismatch" {
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

	result, err := service.Pull(ctx, application.PullOptions{Concurrency: 2})
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

func TestPullLocksConcurrentlyAndSortsResults(t *testing.T) {
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

	result, err := service.Pull(ctx, application.PullOptions{Concurrency: 2, MaxSize: 1000})
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

func TestPullUsesPublisherIntegrityFromCache(t *testing.T) {
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

	result, err := service.Pull(context.Background(), application.PullOptions{Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.count() != 0 || result.Assets[0].Status != "cached" {
		t.Fatalf("unexpected cached lock: requests=%d result=%#v", fetcher.count(), result)
	}
}

func TestRefreshUsesStoredETagForRevalidation(t *testing.T) {
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

	result, err := service.Pull(context.Background(), application.PullOptions{Concurrency: 1, Refresh: true})
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

// interruptedBody delivers part of an asset and then fails, which is what a reset
// connection or an expired stall guard looks like to whatever is reading it.
type interruptedBody struct {
	delivered io.Reader
	err       error
}

func (body *interruptedBody) Read(buffer []byte) (int, error) {
	count, err := body.delivered.Read(buffer)
	if errors.Is(err, io.EOF) {
		return count, body.err
	}
	return count, err
}

func (*interruptedBody) Close() error { return nil }

// TestTransferFailureMidBodyKeepsItsNetworkCode covers the whole path a stalled
// transfer takes: one Put call reads the body and writes the cache, and the
// failure it returns has to still say that the network stopped rather than that
// the asset failed its content check.
func TestTransferFailureMidBodyKeepsItsNetworkCode(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		code string
	}{
		{name: "stall", err: application.ErrStalled, code: "timeout"},
		{name: "refused redirect", err: &application.HostError{Host: "elsewhere.example"}, code: "host_not_trusted"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			manifestPath, lockPath := emptyProject(t)
			fetcher := &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
				return &application.FetchResponse{
					Length: 8,
					Body:   &interruptedBody{delivered: bytes.NewReader([]byte("half")), err: testCase.err},
				}, nil
			}}
			service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)
			_, err := service.Add(context.Background(), application.AddOptions{
				Coordinate: at("asset@1"), URL: "https://example.com/asset", Pin: true,
			})
			if value := fault.As(err); value.Code != testCase.code {
				t.Fatalf("code = %q, want %q (%v)", value.Code, testCase.code, err)
			}
		})
	}
}

func TestNetworkTimeoutHasStableCode(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	fetcher := &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		return nil, application.ErrStalled
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)
	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("asset@1"), URL: "https://example.com/asset", Pin: true,
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

func TestPullLockingHonorsCancellation(t *testing.T) {
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
	if _, err := service.Pull(ctx, application.PullOptions{Concurrency: 1}); fault.As(err).Code != "cancelled" {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func TestVerifyNeedsNoCacheOrFetcher(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	result, err := application.New(manifestPath, lockPath, nil, nil, nil).Verify(context.Background(), application.VerifyOptions{})
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

// sequenceFetcher serves a different body to each request, which is one URL whose bytes change: the rolling source a version bump is the honest answer to.
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
// A test about several versions of one asset needs distinct bytes per version, because a fetcher answering everything with one body makes every version collide, which is a different rule from the one under test.
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

// unlockedProject writes a manifest with no lock file beside it, which is the checkout a first pull is there to settle.
func unlockedProject(t *testing.T, manifest project.Manifest) (string, string) {
	t.Helper()
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	writeManifest(t, manifestPath, manifest)
	return manifestPath, filepath.Join(directory, "dac-lock.json")
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
			// The name the URL spells, which is what a lock DAC wrote carries.
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

func failingFetcher(t *testing.T) *fakeFetcher {
	t.Helper()
	return &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		t.Error("the command made a network request")
		return nil, errors.New("unexpected request")
	}}
}

func TestPullTrustsACacheThatSatisfiesIntegrity(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := unlockedProject(t, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/asset", Integrity: digest.Bytes(content)},
		},
	})
	store := newFakeStore()
	warm(t, store, content)
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)
	result, err := service.Pull(context.Background(), application.PullOptions{Concurrency: 1, MaxSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	if result.Assets[0].Status != "cached" {
		t.Fatalf("unexpected status %q", result.Assets[0].Status)
	}
}

func TestRefreshContactsTheOriginAnyway(t *testing.T) {
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
	result, err := service.Pull(context.Background(), application.PullOptions{
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

func TestRefreshSendsNoConditionalRequest(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	store := newFakeStore()
	warm(t, store, content)
	fetcher := staticFetcher(content)
	service := application.New(manifestPath, lockPath, store, fetcher, nil)
	if _, err := service.Pull(context.Background(), application.PullOptions{
		Concurrency: 1, MaxSize: 100, Refresh: true,
	}); err != nil {
		t.Fatal(err)
	}
	if fetcher.requests[0].ETag != "" {
		t.Fatalf("refresh sent an ETag hint: %q", fetcher.requests[0].ETag)
	}
}

// A pinned asset is settled by its publisher digest, so it never revalidates.
func TestPullRecordsNoETagForAPinnedAsset(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := unlockedProject(t, project.Manifest{
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

	if _, err := service.Pull(context.Background(), application.PullOptions{Concurrency: 1, MaxSize: 100}); err != nil {
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
	manifest, err := project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
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

// The tests below cover what the cache does once an object stops matching the digest that names it.

// seedCorrupt returns a project whose one asset is present in the cache but damaged.
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

func TestPullRepairsACorruptObject(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath, store := seedCorrupt(t, content)
	service := application.New(manifestPath, lockPath, store, staticFetcher(content), nil)

	result, err := service.Pull(context.Background(), application.PullOptions{Concurrency: 1})
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

	_, err := service.Pull(context.Background(), application.PullOptions{Concurrency: 1, Offline: true})
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
	// An object no project references still belongs to the shared cache, so an --all check has to reach it.
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

	_, err := service.Verify(context.Background(), application.VerifyOptions{Concurrency: 1, Refresh: true})
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

	result, err := service.Verify(context.Background(), application.VerifyOptions{Concurrency: 1, Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Refreshed || result.AssetCount != 1 {
		t.Fatalf("unexpected refresh result: %#v", result)
	}
}

// An ETag is a cache hint a server may rotate over identical bytes.
func TestVerifyRefreshIgnoresARotatedETag(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	fetcher := &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		served := response(content)
		served.ETag = "\"rotated\""
		return served, nil
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)

	if _, err := service.Verify(context.Background(), application.VerifyOptions{Concurrency: 1, Refresh: true}); err != nil {
		t.Fatal(err)
	}
}

// A manifest edited without locking it is a different failure from an origin that moved, and it has a different fix.
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

	_, err := service.Verify(context.Background(), application.VerifyOptions{Concurrency: 1, Refresh: true})
	if fault.As(err).Code != "lock_stale" {
		t.Fatalf("expected lock_stale, got %v", err)
	}
}

// The property that lets pull carry the lock command's job without carrying its cost.
func TestPullLeavesAgreeingAssetsAlone(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	before := projecttest.MustRead(t, lockPath)
	store := newFakeStore()
	warm(t, store, content)
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)

	result, err := service.Pull(context.Background(), application.PullOptions{Concurrency: 1})
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

// A project whose lock file has never been written is the state a first pull exists to leave behind, not an error to report.
func TestPullCreatesAMissingLockFile(t *testing.T) {
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

	result, err := service.Pull(context.Background(), application.PullOptions{Concurrency: 1, MaxSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Locked) != 1 || result.Locked[0] != "test/asset@1" {
		t.Fatalf("lock did not report the asset it locked: %#v", result.Locked)
	}
	if !result.Changed {
		t.Fatal("writing a new lock file was not reported as a change")
	}
	// Resolving stored these bytes, so crediting the cache for them would describe a transfer this command had just made as one it avoided.
	if result.Assets[0].Status != "resolved" {
		t.Fatalf("unexpected status %q", result.Assets[0].Status)
	}
	projecttest.Check(t, manifestPath, lockPath)
}

// Writing a lock file means resolving assets, and offline mode resolves nothing, so the
// project with no lock file is the one case a pull cannot settle for itself.
func TestOfflinePullReportsAMissingLockFile(t *testing.T) {
	manifestPath, lockPath := unlockedProject(t, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/asset"},
		},
	})
	service := application.New(manifestPath, lockPath, newFakeStore(), failingFetcher(t), nil)

	_, err := service.Pull(context.Background(), application.PullOptions{Concurrency: 1, Offline: true})
	if code := fault.As(err).Code; code != "lock_missing" {
		t.Fatalf("expected lock_missing, got %q (%v)", code, err)
	}
}

// The two options ask for opposite things, and refusing says so rather than leaving the
// user to work out which one the run quietly ignored.
func TestOfflineRefreshIsRefused(t *testing.T) {
	manifestPath, lockPath := lockedProject(t, []byte("asset bytes"))
	service := application.New(manifestPath, lockPath, newFakeStore(), failingFetcher(t), nil)

	_, err := service.Pull(context.Background(), application.PullOptions{
		Concurrency: 1, Offline: true, Refresh: true,
	})
	if code := fault.As(err).Code; code != "invalid_arguments" {
		t.Fatalf("expected invalid_arguments, got %q (%v)", code, err)
	}
}

// Pull owns reconciliation, so a manifest edit is settled by an ordinary online pull.
func TestPullReconcilesAStaleLock(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/moved"},
		},
	})
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(content), nil)

	result, err := service.Pull(context.Background(), application.PullOptions{Concurrency: 1, MaxSize: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || len(result.Locked) != 1 {
		t.Fatalf("pull did not reconcile the stale lock: %#v", result)
	}
	projecttest.Check(t, manifestPath, lockPath)
}

// Add never creates a missing lock file; the next pull owns that transition.
func TestAddLeavesAMissingLockFileMissing(t *testing.T) {
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
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("add created the lock file: %v", err)
	}
}

// Add is independent of lock health and leaves an already-stale lock byte-for-byte alone.
func TestAddLeavesAHandEditedLockUntouched(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/moved"},
		},
	})
	lockBefore := projecttest.MustRead(t, lockPath)
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(content), nil)

	result, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("other@1"), URL: "https://example.com/other", MaxSize: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Locked) != 0 {
		t.Fatalf("add reported locking assets: %#v", result.Locked)
	}
	if !bytes.Equal(lockBefore, projecttest.MustRead(t, lockPath)) {
		t.Fatal("add changed the stale lock")
	}
}

// Removal neither validates nor rewrites stale lock state.
func TestRemoveLeavesAStaleLockUntouched(t *testing.T) {
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
	lockBefore := projecttest.MustRead(t, lockPath)
	service := application.New(manifestPath, lockPath, newFakeStore(), failingFetcher(t), nil)

	result, err := service.Remove(at("gone@1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Unlocked) != 0 {
		t.Fatalf("remove reported lock work: %#v", result.Unlocked)
	}
	manifest, err := project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := manifest.Assets[at("gone@1")]; exists {
		t.Fatal("remove left the asset in the manifest")
	}
	if !bytes.Equal(lockBefore, projecttest.MustRead(t, lockPath)) {
		t.Fatal("remove changed the stale lock")
	}
}

// Remove does not need lock state at all, so a damaged lock cannot block a local manifest edit.
func TestRemoveIgnoresAnInvalidLock(t *testing.T) {
	manifestPath, lockPath := lockedProject(t, []byte("asset bytes"))
	invalid := []byte("not JSON\n")
	if err := os.WriteFile(lockPath, invalid, 0o644); err != nil {
		t.Fatal(err)
	}
	service := application.New(manifestPath, lockPath, nil, nil, nil)

	if _, err := service.Remove(at("asset@1")); err != nil {
		t.Fatalf("invalid lock blocked remove: %v", err)
	}
	if !bytes.Equal(invalid, projecttest.MustRead(t, lockPath)) {
		t.Fatal("remove rewrote the invalid lock")
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
	lockBefore := projecttest.MustRead(t, lockPath)

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
	manifest, err := project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Assets[at("asset@1")].Integrity != value {
		t.Fatalf("the manifest was not pinned: %#v", manifest.Assets[at("asset@1")])
	}
	if !bytes.Equal(lockBefore, projecttest.MustRead(t, lockPath)) {
		t.Fatal("add --pin changed the lock file")
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

func TestAddAllowsTwoVersionsOfTheSameBytes(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(content), nil)
	lockBefore := projecttest.MustRead(t, lockPath)

	_, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("asset@2"), URL: "https://example.com/asset", MaxSize: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Assets) != 2 {
		t.Fatalf("manifest does not contain both versions: %#v", manifest.Assets)
	}
	if !bytes.Equal(lockBefore, projecttest.MustRead(t, lockPath)) {
		t.Fatal("add changed the lock file")
	}
}

func TestRefreshUpdatesChangedBytes(t *testing.T) {
	manifestPath, lockPath := lockedProject(t, []byte("asset bytes"))
	moved := []byte("moved bytes")
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher(moved), nil)

	result, err := service.Pull(context.Background(), application.PullOptions{
		Concurrency: 1, MaxSize: 1 << 20, Refresh: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Changed || lock.Assets[at("asset@1")].Digest != digest.Bytes(moved) {
		t.Fatalf("refresh did not update the lock: %#v %#v", result, lock.Assets[at("asset@1")])
	}
}

// Two versions of one asset served from one URL A shared source warning tells users that one URL can restore only its current bytes.
func TestAddReportsASharedSourceURL(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	// The source changes bytes between versions but keeps the same URL.
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

// namedFetcher serves one body under a name the origin supplies, which is what a Content-Disposition header amounts to by the time it reaches the service.
func namedFetcher(content []byte, name string) *fakeFetcher {
	return &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		value := response(content)
		value.Filename = name
		return value, nil
	}}
}

// unlockedProject writes a lock entry with no file name: a project locked by a DAC from before the field existed.
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

// settleProject moves manifest-only setup through the sole lock-writing operation.
func settleProject(t *testing.T, service *application.Service) {
	t.Helper()
	if _, err := service.Pull(context.Background(), application.PullOptions{Concurrency: 1, MaxSize: 1 << 20}); err != nil {
		t.Fatal(err)
	}
}

// The name an origin supplies is the whole point of the field: a URL ending in an opaque endpoint spells nothing useful, and the header is the only place the real name appears.
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
	settleProject(t, service)
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
	settleProject(t, service)
	if name := lockedFilename(t, lockPath); name != "database.bin" {
		t.Fatalf("lock recorded %q, want the name the URL spells", name)
	}
}

// A name an origin supplies that is not one path element is refused rather than repaired, and the URL answers instead.
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
	settleProject(t, service)
	if name := lockedFilename(t, lockPath); name != "database.bin" {
		t.Fatalf("lock recorded %q, want the URL name instead of the escaping one", name)
	}
}

// A missing name never makes a lock look stale.
func TestALockWithNoFilenameStillDescribesItsManifest(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := unlockedNameProject(t, content)
	store := newFakeStore()
	warm(t, store, content)
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)

	result, err := service.Pull(context.Background(), application.PullOptions{Concurrency: 1})
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

// A manifest that repoints an asset replaces the source the old name described, so the name goes with it rather than following the bytes.
func TestResolveDropsTheOldNameWhenTheURLMoves(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	service := application.New(manifestPath, lockPath, newFakeStore(), pathFetcher(), nil)

	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("asset@1"), URL: "https://example.com/old/first.bin", MaxSize: 1 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	settleProject(t, service)
	if name := lockedFilename(t, lockPath); name != "first.bin" {
		t.Fatalf("lock recorded %q", name)
	}

	writeManifest(t, manifestPath, project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/new/second.bin"},
		},
	})
	if _, err := service.Pull(context.Background(), application.PullOptions{
		Concurrency: 1, Refresh: true, MaxSize: 1 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	if name := lockedFilename(t, lockPath); name != "second.bin" {
		t.Fatalf("lock kept %q after the URL moved", name)
	}
}

// A 304 says these are the bytes the lock already describes, so it must not rename them.
func TestNotModifiedKeepsTheRecordedFilename(t *testing.T) {
	content := []byte("cached")
	manifestPath, lockPath := lockedProject(t, content)
	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	asset := lock.Assets[at("asset@1")]
	asset.ETag = "\"old\""
	// A name only a Content-Disposition header could have produced: the URL of this fixture spells "asset".
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
			// What the adapter reports for a 304 with no header: the name the URL spells, which is worse than the one already recorded.
			Filename: "asset",
			Body:     io.NopCloser(bytes.NewReader(nil)),
		}, nil
	}}
	service := application.New(manifestPath, lockPath, store, fetcher, nil)

	result, err := service.Pull(context.Background(), application.PullOptions{
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

// The same response does fill in an entry that has no name, which is how a lock from before the field existed migrates through a refresh.
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

	if _, err := service.Pull(context.Background(), application.PullOptions{
		Concurrency: 1, Refresh: true,
	}); err != nil {
		t.Fatal(err)
	}
	if name := lockedFilename(t, lockPath); name != "database.bin" {
		t.Fatalf("the backfilled name is %q", name)
	}
}

// A name the manifest declares is the project's own answer, so it beats the one the origin supplies -- the case the flag exists for, since an origin that sends a useful Content-Disposition header is the one whose name is hardest to override any other way.
func TestAddNameOverridesTheNameTheOriginSupplies(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := emptyProject(t)
	service := application.New(manifestPath, lockPath, newFakeStore(),
		namedFetcher(content, "database.bin"), nil)

	result, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("asset@1"), URL: "https://example.com/geo/database.bin",
		Filename: "geo.db", MaxSize: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Filename != "geo.db" {
		t.Fatalf("add reported the name %q", result.Filename)
	}
	settleProject(t, service)
	if name := lockedFilename(t, lockPath); name != "geo.db" {
		t.Fatalf("lock recorded %q, want the declared name", name)
	}
	// The manifest is where the declaration lives, because it is source intent rather than something a resolution found.
	manifest, _ := projecttest.Check(t, manifestPath, lockPath)
	if declared := manifest.Assets[at("asset@1")].Filename; declared != "geo.db" {
		t.Fatalf("the manifest declares %q", declared)
	}
}

// An add that declares no name leaves every naming decision where it was, which is the whole compatibility claim for the flag.
func TestAddWithoutANameLeavesTheOriginNaming(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := emptyProject(t)
	service := application.New(manifestPath, lockPath, newFakeStore(),
		namedFetcher(content, "database.bin"), nil)

	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("asset@1"), URL: "https://example.com/download?id=1234", MaxSize: 1 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	settleProject(t, service)
	if name := lockedFilename(t, lockPath); name != "database.bin" {
		t.Fatalf("lock recorded %q, want the supplied name", name)
	}
	manifest, _ := projecttest.Check(t, manifestPath, lockPath)
	if declared := manifest.Assets[at("asset@1")].Filename; declared != "" {
		t.Fatalf("an add that named nothing wrote %q to the manifest", declared)
	}
}

// An offline add writes only the manifest, and the declaration is a manifest field, so it is the one part of the asset that is settled with no network.
func TestOfflineAddRecordsTheDeclaredName(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	service := application.New(manifestPath, lockPath, newFakeStore(), failingFetcher(t), nil)

	result, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("asset@1"), URL: "https://example.com/geo/database.bin",
		Filename: "geo.db", Offline: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Filename != "geo.db" || result.Status != "unlocked" {
		t.Fatalf("unexpected offline add result: %#v", result.Asset)
	}
	manifest, err := project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if declared := manifest.Assets[at("asset@1")].Filename; declared != "geo.db" {
		t.Fatalf("the manifest declares %q", declared)
	}
}

// A name that is not one path element is refused rather than repaired, and the add fails: a name somebody typed is a decision, so DAC either records the one asked for or says it will not.
func TestAddRefusesANameThatIsNotOnePathElement(t *testing.T) {
	manifestPath, lockPath := emptyProject(t)
	beforeManifest := projecttest.MustRead(t, manifestPath)
	beforeLock := projecttest.MustRead(t, lockPath)
	service := application.New(manifestPath, lockPath, newFakeStore(), failingFetcher(t), nil)

	for _, name := range []string{"../../etc/passwd", "geo/db.bin", "..", "-rf", "bad\x00name", strings.Repeat("x", 256)} {
		_, err := service.Add(context.Background(), application.AddOptions{
			Coordinate: at("asset@1"), URL: "https://example.com/geo/database.bin",
			Filename: name, Offline: true,
		})
		if fault.As(err).Code != "invalid_arguments" {
			t.Fatalf("add accepted the name %q: %v", name, err)
		}
	}
	assertFilesEqual(t, manifestPath, lockPath, beforeManifest, beforeLock)
}

// A refresh that ends in a 304 keeps the declared name too.
func TestNotModifiedKeepsTheDeclaredName(t *testing.T) {
	content := []byte("cached")
	manifestPath, lockPath := emptyProject(t)
	manifest := project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			at("asset@1"): {URL: "https://example.com/geo/database.bin", Filename: "geo.db"},
		},
	}
	lock, err := project.NewLock(manifest, map[coord.Coordinate]project.LockAsset{
		at("asset@1"): {
			URL:      "https://example.com/geo/database.bin",
			Digest:   digest.Bytes(content),
			Size:     int64(len(content)),
			ETag:     "\"old\"",
			Filename: "geo.db",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := project.WritePair(manifestPath, lockPath, manifest, lock); err != nil {
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

	if _, err := service.Pull(context.Background(), application.PullOptions{
		Concurrency: 1, Refresh: true,
	}); err != nil {
		t.Fatal(err)
	}
	if name := lockedFilename(t, lockPath); name != "geo.db" {
		t.Fatalf("a not-modified response changed the declared name to %q", name)
	}
}

// A pinned asset the cache already holds is answered without a request, and that path builds its lock entry from nothing the origin said.
func TestADeclaredNameReachesACachedResolution(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := emptyProject(t)
	store := newFakeStore()
	warm(t, store, content)
	service := application.New(manifestPath, lockPath, store, failingFetcher(t), nil)

	if _, err := service.Add(context.Background(), application.AddOptions{
		Coordinate: at("asset@1"), URL: "https://example.com/download?id=1234",
		Integrity: digest.Bytes(content), Filename: "geo.db", MaxSize: 1 << 20,
	}); err != nil {
		t.Fatal(err)
	}
	settleProject(t, service)
	if name := lockedFilename(t, lockPath); name != "geo.db" {
		t.Fatalf("a cached resolution recorded %q", name)
	}
}
