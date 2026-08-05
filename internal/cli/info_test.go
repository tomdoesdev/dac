package cli

import (
	"os"
	"testing"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/project"
)

func TestInfoCommandCombinesManifestAndCacheState(t *testing.T) {
	paths := newProject(t)
	manifest := project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			coord.MustParse("app/first@1"):  {URL: "https://one.example.com/asset"},
			coord.MustParse("app/second@2"): {URL: "https://two.example.com/asset"},
			coord.MustParse("app/third@3"):  {URL: "https://three.example.com/asset"},
		},
	}
	if err := project.Write(paths.manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	human := run(t, appendArgs(paths.base, "info"))
	expected := "app/first@1\n" +
		"source: https://one.example.com/asset\n" +
		"trust: untrusted\n" +
		"lock: missing\n" +
		"cache: unavailable\n\n" +
		"app/second@2\n" +
		"source: https://two.example.com/asset\n" +
		"trust: untrusted\n" +
		"lock: missing\n" +
		"cache: unavailable\n\n" +
		"app/third@3\n" +
		"source: https://three.example.com/asset\n" +
		"trust: untrusted\n" +
		"lock: missing\n" +
		"cache: unavailable\n"
	if human.status != ExitOK || human.stdout != expected || human.stderr != "" {
		t.Fatalf("unexpected human info result: %#v", human)
	}

	jsonResult := runJSON(t, appendArgs(paths.base, "info"))
	assertSuccess(t, jsonResult, "info")
	data := jsonResult.value["data"].(map[string]any)
	summary := data["summary"].(map[string]any)
	if summary["assetCount"] != float64(3) || summary["cachedCount"] != float64(0) ||
		summary["corruptCount"] != float64(0) || summary["lockStatus"] != "missing" {
		t.Fatalf("unexpected info counts: %#v", data)
	}
	if _, found := summary["blockedCount"]; found {
		t.Fatalf("info summary contains removed blockedCount: %#v", summary)
	}
	assets := data["assets"].([]any)
	first := assets[0].(map[string]any)
	if first["name"] != "first" || first["cacheStatus"] != "unavailable" ||
		first["sourceUrl"] != "https://one.example.com/asset" {
		t.Fatalf("unexpected first asset: %#v", first)
	}
	for _, field := range []string{"requestUrl", "requestStatus", "rewritten"} {
		if _, found := first[field]; found {
			t.Fatalf("info asset contains removed %s: %#v", field, first)
		}
	}

	single := runJSON(t, appendArgs(paths.base, "info", "app/third@3"))
	assertSuccess(t, single, "info")
	singleData := single.value["data"].(map[string]any)
	singleAssets := singleData["assets"].([]any)
	singleSummary := singleData["summary"].(map[string]any)
	if singleSummary["assetCount"] != float64(1) ||
		len(singleAssets) != 1 || singleAssets[0].(map[string]any)["coordinate"] != "app/third@3" {
		t.Fatalf("unexpected single info result: %#v", singleData)
	}
	singleHuman := run(t, appendArgs(paths.base, "info", "app/third@3"))
	expectedSingle := "app/third@3\n" +
		"source: https://three.example.com/asset\n" +
		"trust: untrusted\n" +
		"lock: missing\n" +
		"cache: unavailable\n"
	if singleHuman.status != ExitOK || singleHuman.stdout != expectedSingle || singleHuman.stderr != "" {
		t.Fatalf("unexpected single human info: %#v", singleHuman)
	}
	assertError(t, runJSON(t, appendArgs(paths.base, "info", "app/third@2")), "asset_unknown")

	group := runJSON(t, appendArgs(paths.base, "info", "app/third"))
	assertSuccess(t, group, "info")
	groupAssets := group.value["data"].(map[string]any)["assets"].([]any)
	if len(groupAssets) != 1 || groupAssets[0].(map[string]any)["coordinate"] != "app/third@3" {
		t.Fatalf("unexpected asset filter result: %#v", groupAssets)
	}
	assertError(t, runJSON(t, appendArgs(paths.base, "info", "app/absent")), "asset_unknown")
}

func TestInfoReportsAStaleLockWithoutCacheData(t *testing.T) {
	paths := newProject(t)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "app/geo@1",
		"https://example.com/geo", "--offline")), "add")

	result := runJSON(t, appendArgs(paths.base, "info"))
	assertSuccess(t, result, "info")
	data := result.value["data"].(map[string]any)
	asset := data["assets"].([]any)[0].(map[string]any)
	if data["summary"].(map[string]any)["lockStatus"] != "stale" || asset["cacheStatus"] != "unavailable" {
		t.Fatalf("unexpected stale info result: %#v", data)
	}
	for _, field := range []string{"digest", "size", "path"} {
		if _, exists := asset[field]; exists {
			t.Fatalf("stale info contains %s: %#v", field, asset)
		}
	}
}

func TestInfoRejectsAnInvalidLock(t *testing.T) {
	paths := newProject(t)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	if err := os.WriteFile(paths.lockPath, []byte("not JSON\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertError(t, runJSON(t, appendArgs(paths.base, "info")), "lock_invalid")
}
