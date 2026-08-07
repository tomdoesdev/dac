package dac_test

import (
	"testing"

	"github.com/tomdoesdev/dac/internal/dac"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fault"
)

func TestInfoCombinesProjectAndCacheState(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	store := newFakeStore()
	warm(t, store, content)
	service := dac.New(manifestPath, lockPath, store, failingFetcher(t), nil)
	result, err := service.Info(dac.InfoOptions{Selection: dac.ExactSelection(at("asset@1"))})
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.AssetCount != 1 || result.Summary.CachedCount != 1 || result.Summary.CorruptCount != 0 ||
		result.Summary.LockStatus != "current" {
		t.Fatalf("unexpected info counts: %#v", result.Summary)
	}
	asset := result.Assets[0]
	if asset.SourceURL != "https://example.com/asset" || asset.CacheStatus != "cached" ||
		asset.Digest != digest.Bytes(content) || asset.Size == nil || *asset.Size != int64(len(content)) || asset.Path == "" {
		t.Fatalf("unexpected info asset: %#v", asset)
	}
	_, err = service.Info(dac.InfoOptions{Selection: dac.ExactSelection(at("asset@2"))})
	unknown := fault.As(err)
	if unknown.Code != "asset_unknown" {
		t.Fatalf("expected asset_unknown, got %v", unknown)
	}
	if versions, _ := unknown.Details["versions"].([]string); len(versions) != 1 || versions[0] != "1" {
		t.Fatalf("asset_unknown did not name the project versions: %#v", unknown.Details)
	}
}

func TestInfoReportsACorruptObject(t *testing.T) {
	manifestPath, lockPath, store := seedCorrupt(t, []byte("asset bytes"))
	service := dac.New(manifestPath, lockPath, store, failingFetcher(t), nil)

	result, err := service.Info(dac.InfoOptions{})
	if err != nil {
		t.Fatalf("info should describe damage: %v", err)
	}
	if result.Assets[0].CacheStatus != "corrupt" || result.Summary.CorruptCount != 1 {
		t.Fatalf("unexpected info result: %#v", result)
	}
}

func TestInfoReportsTheLockedFilename(t *testing.T) {
	content := []byte("asset bytes")
	manifestPath, lockPath := lockedProject(t, content)
	store := newFakeStore()
	warm(t, store, content)
	service := dac.New(manifestPath, lockPath, store, failingFetcher(t), nil)

	result, err := service.Info(dac.InfoOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Assets) != 1 || result.Assets[0].Filename != "asset" {
		t.Fatalf("info did not report the locked file name: %#v", result.Assets)
	}
}
