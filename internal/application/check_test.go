package application_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
)

const checkLastModified = "Wed, 21 Oct 2015 07:28:00 GMT"

type fakeProber struct {
	mutex    sync.Mutex
	requests []application.ProbeRequest
	probe    func(context.Context, application.ProbeRequest) (*application.ProbeResponse, error)
}

// Probe records a HEAD-like request and delegates its answer to the test.
func (prober *fakeProber) Probe(ctx context.Context, request application.ProbeRequest) (*application.ProbeResponse, error) {
	prober.mutex.Lock()
	prober.requests = append(prober.requests, request)
	prober.mutex.Unlock()
	return prober.probe(ctx, request)
}

// count reports how many metadata requests the prober observed.
func (prober *fakeProber) count() int {
	prober.mutex.Lock()
	defer prober.mutex.Unlock()
	return len(prober.requests)
}

type probeRequestError struct {
	status int
}

func (value *probeRequestError) Error() string      { return http.StatusText(value.status) }
func (value *probeRequestError) RequestURL() string { return "https://example.com/asset" }
func (value *probeRequestError) StatusCode() int    { return value.status }

type forbiddenCatalog struct{ t *testing.T }

func (catalog forbiddenCatalog) Note([]application.CatalogEntry) {
	catalog.t.Fatal("offline check updated the catalog")
}
func (catalog forbiddenCatalog) Forget([]string) {
	catalog.t.Fatal("offline check updated the catalog")
}
func (catalog forbiddenCatalog) Describe(string) (application.CatalogRecord, bool) {
	catalog.t.Fatal("offline check read the catalog")
	return application.CatalogRecord{}, false
}

func TestCheckOfflineReadsOnlyStrictProjectFiles(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	service := application.New(manifestPath, lockPath, nil, nil, nil)
	service.Catalog = forbiddenCatalog{t: t}

	result, err := service.Check(context.Background(), application.CheckOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Upstream || result.AssetCount != 1 || result.ManifestDigest == "" {
		t.Fatalf("unexpected offline result: %#v", result)
	}
}

func TestCheckOfflineRetainsProjectFailureClassifications(t *testing.T) {
	tests := map[string]struct {
		prepare func(*testing.T, string, string)
		code    string
	}{
		"missing lock": {
			prepare: func(t *testing.T, _, lockPath string) {
				if err := os.Remove(lockPath); err != nil {
					t.Fatal(err)
				}
			},
			code: "lock_missing",
		},
		"malformed lock": {
			prepare: func(t *testing.T, _, lockPath string) {
				if err := os.WriteFile(lockPath, []byte("{"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			code: "lock_invalid",
		},
		"stale lock": {
			prepare: func(t *testing.T, manifestPath, _ string) {
				writeManifest(t, manifestPath, project.Manifest{
					SchemaVersion: project.ManifestVersion,
					Assets: map[coord.Coordinate]project.Asset{
						coord.MustParse("test/other@1"): {URL: "https://example.com/other"},
					},
				})
			},
			code: "lock_stale",
		},
	}
	for name, testCase := range tests {
		t.Run(name, func(t *testing.T) {
			manifestPath, lockPath := lockedProject(t, []byte("asset bytes"))
			testCase.prepare(t, manifestPath, lockPath)
			_, err := application.New(manifestPath, lockPath, nil, nil, nil).Check(context.Background(), application.CheckOptions{})
			if code := fault.As(err).Code; code != testCase.code {
				t.Fatalf("code=%q, want %q (%v)", code, testCase.code, err)
			}
		})
	}
}

func TestCheckUpstreamUsesMatchingStoredValidatorsWithoutGET(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	setCheckValidators(t, lockPath, "W/\"known\"", checkLastModified)
	fetcher := &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		return nil, errors.New("GET should not have been sent")
	}}
	prober := &fakeProber{probe: func(context.Context, application.ProbeRequest) (*application.ProbeResponse, error) {
		return &application.ProbeResponse{
			ETag:         "W/\"known\"",
			LastModified: checkLastModified,
			Length:       int64(len(content)),
		}, nil
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)
	service.Prober = prober

	result, err := service.Check(context.Background(), application.CheckOptions{Concurrency: 1, Upstream: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Downloaded != 0 || fetcher.count() != 0 || prober.count() != 1 {
		t.Fatalf("unexpected probe result: %#v, HEAD=%d GET=%d", result, prober.count(), fetcher.count())
	}
}

func TestCheckUpstreamIgnoresANewValidatorWhenKnownHintsMatch(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	setCheckValidators(t, lockPath, "\"known\"", "")
	fetcher := &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		return nil, errors.New("GET should not have been sent")
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)
	service.Prober = &fakeProber{probe: func(context.Context, application.ProbeRequest) (*application.ProbeResponse, error) {
		return &application.ProbeResponse{
			ETag:         "\"known\"",
			LastModified: checkLastModified,
			Length:       int64(len(content)),
		}, nil
	}}

	result, err := service.Check(context.Background(), application.CheckOptions{Concurrency: 1, Upstream: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Downloaded != 0 || fetcher.count() != 0 {
		t.Fatalf("new validator triggered a GET: %#v", result)
	}
}

func TestCheckUpstreamDownloadsWhenAKnownHintChangesOrDisappears(t *testing.T) {
	content := []byte("asset bytes")
	tests := map[string]application.ProbeResponse{
		"etag changed":          {ETag: "\"new\"", LastModified: checkLastModified, Length: int64(len(content))},
		"etag missing":          {LastModified: checkLastModified, Length: int64(len(content))},
		"last modified changed": {ETag: "\"known\"", LastModified: "Thu, 22 Oct 2015 07:28:00 GMT", Length: int64(len(content))},
		"last modified missing": {ETag: "\"known\"", Length: int64(len(content))},
		"length changed":        {ETag: "\"known\"", LastModified: checkLastModified, Length: int64(len(content) + 1)},
	}
	for name, answer := range tests {
		t.Run(name, func(t *testing.T) {
			manifestPath, lockPath := lockedProject(t, content)
			setCheckValidators(t, lockPath, "\"known\"", checkLastModified)
			fetcher := staticFetcher(content)
			prober := &fakeProber{probe: func(context.Context, application.ProbeRequest) (*application.ProbeResponse, error) {
				return &answer, nil
			}}
			service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)
			service.Prober = prober

			result, err := service.Check(context.Background(), application.CheckOptions{Concurrency: 1, Upstream: true})
			if err != nil {
				t.Fatal(err)
			}
			if result.Downloaded != 1 || fetcher.count() != 1 {
				t.Fatalf("changed hint did not force one GET: %#v, requests=%d", result, fetcher.count())
			}
		})
	}
}

func TestCheckUpstreamFallsBackWhenHEADIsUnsupported(t *testing.T) {
	for _, status := range []int{http.StatusMethodNotAllowed, http.StatusNotImplemented} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			content := []byte("asset bytes")
			manifestPath, lockPath := lockedProject(t, content)
			setCheckValidators(t, lockPath, "\"known\"", "")
			fetcher := staticFetcher(content)
			service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)
			service.Prober = &fakeProber{probe: func(context.Context, application.ProbeRequest) (*application.ProbeResponse, error) {
				return nil, &probeRequestError{status: status}
			}}

			result, err := service.Check(context.Background(), application.CheckOptions{Concurrency: 1, Upstream: true})
			if err != nil {
				t.Fatal(err)
			}
			if result.Downloaded != 1 || fetcher.count() != 1 {
				t.Fatalf("HEAD %d did not fall back: %#v", status, result)
			}
		})
	}
}

func TestCheckUpstreamDownloadsAssetsWithoutValidatorsAndNeverCachesThem(t *testing.T) {
	content := []byte("shared bytes")
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	one := coord.MustParse("test/unpinned@1")
	two := coord.MustParse("test/pinned@1")
	manifest := project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			one: {URL: "https://example.com/one"},
			two: {URL: "https://example.com/two", Integrity: digest.Bytes(content)},
		},
	}
	writeManifest(t, manifestPath, manifest)
	lock, err := project.NewLock(manifest, map[coord.Coordinate]project.LockAsset{
		one: {URL: manifest.Assets[one].URL, Digest: digest.Bytes(content), Size: int64(len(content))},
		two: {URL: manifest.Assets[two].URL, Digest: digest.Bytes(content), Size: int64(len(content))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := project.Write(lockPath, lock); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	fetcher := staticFetcher(content)
	service := application.New(manifestPath, lockPath, store, fetcher, nil)

	result, err := service.Check(context.Background(), application.CheckOptions{Concurrency: 2, Upstream: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Downloaded != 2 || fetcher.count() != 2 || len(store.objects) != 0 {
		t.Fatalf("unexpected no-validator check: %#v, GET=%d cache=%d", result, fetcher.count(), len(store.objects))
	}
}

func TestCheckUpstreamAggregatesChangedBytesAsLockDrift(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher([]byte("changed bytes")), nil)

	_, err := service.Check(context.Background(), application.CheckOptions{Concurrency: 1, Upstream: true})
	value := fault.As(err)
	if value.Code != "lock_drift" {
		t.Fatalf("expected lock_drift, got %v", err)
	}
	assets, _ := value.Details["assets"].([]string)
	if len(assets) != 1 || assets[0] != "test/asset@1" {
		t.Fatalf("unexpected drift details: %#v", value.Details)
	}
}

// setCheckValidators adds canonical hints to the single-asset lock fixture.
func setCheckValidators(t *testing.T, lockPath, etag, lastModified string) {
	t.Helper()
	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	name := coord.MustParse("test/asset@1")
	asset := lock.Assets[name]
	asset.ETag = etag
	asset.LastModified = lastModified
	lock.Assets[name] = asset
	if err := project.Write(lockPath, lock); err != nil {
		t.Fatal(err)
	}
}
