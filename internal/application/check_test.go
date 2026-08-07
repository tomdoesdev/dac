package application_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestCheckMetadataUsesMatchingStoredValidatorsWithoutGET(t *testing.T) {
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

	result, err := service.Check(context.Background(), application.CheckOptions{Concurrency: 1, Mode: application.CheckMetadata})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Upstream || result.Verified || result.Downloaded != 0 || fetcher.count() != 0 || prober.count() != 1 {
		t.Fatalf("unexpected probe result: %#v, HEAD=%d GET=%d", result, prober.count(), fetcher.count())
	}
}

func TestCheckMetadataIgnoresANewValidatorWhenNoHintWasLocked(t *testing.T) {
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

	result, err := service.Check(context.Background(), application.CheckOptions{Concurrency: 1, Mode: application.CheckMetadata})
	if err != nil {
		t.Fatal(err)
	}
	if result.Downloaded != 0 || fetcher.count() != 0 {
		t.Fatalf("new validator triggered a GET: %#v", result)
	}
}

func TestCheckMetadataAggregatesChangedHintsWithoutGET(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	setCheckValidators(t, lockPath, "\"known\"", checkLastModified)
	fetcher := &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		return nil, errors.New("GET should not have been sent")
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)
	service.Prober = &fakeProber{probe: func(context.Context, application.ProbeRequest) (*application.ProbeResponse, error) {
		return &application.ProbeResponse{ETag: "\"new\"", Length: int64(len(content) + 1)}, nil
	}}

	_, err := service.Check(context.Background(), application.CheckOptions{Concurrency: 1, Mode: application.CheckMetadata})
	value := fault.As(err)
	if value.Code != "metadata_mismatch" || fetcher.count() != 0 {
		t.Fatalf("unexpected metadata error: %v, GET=%d", err, fetcher.count())
	}
	assets, _ := value.Details["assets"].([]string)
	if len(assets) != 1 || assets[0] != "test/asset@1" || !strings.Contains(value.Message, "test/asset@1") {
		t.Fatalf("metadata error did not identify the asset: %#v", value)
	}
}

func TestCheckMetadataSelectsExactCoordinatesOnce(t *testing.T) {
	manifestPath, lockPath, _ := multiAssetProject(t)
	fetcher := &fakeFetcher{fetch: func(context.Context, application.FetchRequest) (*application.FetchResponse, error) {
		return nil, errors.New("GET should not have been sent")
	}}
	prober := &fakeProber{probe: func(context.Context, application.ProbeRequest) (*application.ProbeResponse, error) {
		return &application.ProbeResponse{Length: application.Unknown}, nil
	}}
	service := application.New(manifestPath, lockPath, newFakeStore(), fetcher, nil)
	service.Prober = prober

	result, err := service.Check(context.Background(), application.CheckOptions{
		Concurrency: 1,
		Mode:        application.CheckMetadata,
		Assets: []coord.Coordinate{
			at("geo@1"), at("geo@1"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ProjectCount != 3 || result.AssetCount != 1 || prober.count() != 1 || fetcher.count() != 0 {
		t.Fatalf("unexpected selected metadata result: %#v, HEAD=%d GET=%d", result, prober.count(), fetcher.count())
	}

	_, err = service.Check(context.Background(), application.CheckOptions{
		Mode:   application.CheckMetadata,
		Assets: []coord.Coordinate{at("geo@9")},
	})
	if fault.As(err).Code != "asset_unknown" || prober.count() != 1 {
		t.Fatalf("unknown asset made a request: %v, HEAD=%d", err, prober.count())
	}
}

func TestCheckBytesDownloadsPinnedAssetsWithoutProbingOrCaching(t *testing.T) {
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
	fetcher := &fakeFetcher{fetch: func(_ context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
		if request.ETag != "" || request.LastModified != "" {
			return nil, fmt.Errorf("byte verification sent validators: %#v", request)
		}
		return response(content), nil
	}}
	service := application.New(manifestPath, lockPath, store, fetcher, nil)
	service.Prober = &fakeProber{probe: func(context.Context, application.ProbeRequest) (*application.ProbeResponse, error) {
		return nil, errors.New("HEAD should not have been sent")
	}}

	result, err := service.Check(context.Background(), application.CheckOptions{Concurrency: 2, Mode: application.CheckBytes})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Verified || result.Downloaded != 2 || fetcher.count() != 2 || service.Prober.(*fakeProber).count() != 0 || len(store.objects) != 0 {
		t.Fatalf("unexpected byte check: %#v, GET=%d cache=%d", result, fetcher.count(), len(store.objects))
	}
}

func TestCheckBytesAggregatesChangedBytesAsLockDrift(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	service := application.New(manifestPath, lockPath, newFakeStore(), staticFetcher([]byte("changed bytes")), nil)

	_, err := service.Check(context.Background(), application.CheckOptions{Concurrency: 1, Mode: application.CheckBytes})
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
