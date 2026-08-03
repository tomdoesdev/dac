package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tom/dac/internal/cache"
	"github.com/tom/dac/internal/digest"
	"github.com/tom/dac/internal/project"
	"github.com/tom/dac/internal/projecttest"
)

type invocation struct {
	status int
	stdout string
	stderr string
	value  map[string]any
}

func TestCommandLifecycle(t *testing.T) {
	content := []byte("mini dac asset")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("ETag", "\"asset-1\"")
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	cacheRoot := filepath.Join(directory, "cache")
	base := []string{"--manifest", manifestPath, "--lock", lockPath, "--cache-dir", cacheRoot}

	result := runJSON(t, appendArgs(base, "init"))
	assertSuccess(t, result, "init")
	projecttest.Check(t, manifestPath, lockPath)

	result = runJSON(t, appendArgs(base, "add", "geo@2026.08", "--source", server.URL, "--progress=false"))
	assertSuccess(t, result, "add")
	if strings.Contains(result.stdout, "Added") || strings.Contains(result.stdout, "start ") {
		t.Fatalf("human output reached stdout: %q", result.stdout)
	}
	if result.stderr != "" {
		t.Fatalf("JSON add wrote stderr: %q", result.stderr)
	}
	manifest, lock := projecttest.Check(t, manifestPath, lockPath)
	if manifest.Assets["geo"].Version != "2026.08" || lock.Assets["geo"].Digest == "" {
		t.Fatalf("add did not update both files: %#v %#v", manifest, lock)
	}

	result = runJSON(t, appendArgs(base, "info"))
	assertSuccess(t, result, "info")
	infoData := result.value["data"].(map[string]any)
	if infoData["lockStatus"] != "current" || infoData["assetCount"] != float64(1) || infoData["cachedCount"] != float64(1) {
		t.Fatalf("unexpected info result: %#v", infoData)
	}
	infoAsset := infoData["assets"].([]any)[0].(map[string]any)
	if infoAsset["name"] != "geo" || infoAsset["sourceUrl"] != server.URL || infoAsset["requestUrl"] != server.URL ||
		infoAsset["requestStatus"] != "allowed" || infoAsset["rewritten"] != false || infoAsset["cacheStatus"] != "cached" ||
		infoAsset["digest"] != lock.Assets["geo"].Digest || infoAsset["size"] != float64(len(content)) || infoAsset["path"] == "" {
		t.Fatalf("unexpected info asset: %#v", infoAsset)
	}
	result = runJSON(t, appendArgs(base, "verify"))
	assertSuccess(t, result, "verify")
	result = runJSON(t, appendArgs(base, "lock", "--progress=false", "--concurrency", "1"))
	assertSuccess(t, result, "lock")

	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	store := cache.New(cacheRoot)
	objectPath, err := store.Path(lock.Assets["geo"].Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(objectPath); err != nil {
		t.Fatal(err)
	}
	missingInfo := runJSON(t, appendArgs(base, "info", "geo@2026.08"))
	assertSuccess(t, missingInfo, "info")
	missingAsset := missingInfo.value["data"].(map[string]any)["assets"].([]any)[0].(map[string]any)
	if missingAsset["cacheStatus"] != "missing" || missingAsset["digest"] != lock.Assets["geo"].Digest {
		t.Fatalf("unexpected missing cache info: %#v", missingAsset)
	}
	if _, exists := missingAsset["path"]; exists {
		t.Fatalf("missing cache info contains a path: %#v", missingAsset)
	}
	result = runJSON(t, appendArgs(base, "pull", "--concurrency", "1"))
	assertSuccess(t, result, "pull")
	if result.stderr != "" {
		t.Fatalf("JSON pull wrote progress: %q", result.stderr)
	}

	if err := os.Remove(objectPath); err != nil {
		t.Fatal(err)
	}
	result = runJSONArgs(t, appendArgs(base, "pull", "-j", "--concurrency", "1"))
	assertSuccess(t, result, "pull")
	if result.stderr != "" {
		t.Fatalf("-j pull wrote progress: %q", result.stderr)
	}

	if err := os.Remove(objectPath); err != nil {
		t.Fatal(err)
	}
	humanPull := run(t, appendArgs(base, "pull", "--concurrency", "1"))
	if humanPull.status != ExitOK || humanPull.stdout != "Pulled 1 asset.\n" {
		t.Fatalf("unexpected human pull result: %#v", humanPull)
	}
	if !strings.Contains(humanPull.stderr, "start geo ") || !strings.Contains(humanPull.stderr, "done geo downloaded") {
		t.Fatalf("human pull progress is incomplete: %q", humanPull.stderr)
	}

	human := run(t, appendArgs(base, "info"))
	expectedInfo := "geo@2026.08\n" +
		"source: " + server.URL + "\n" +
		"request: " + server.URL + "\n" +
		"policy: allowed\n" +
		"lock: current\n" +
		"cache: cached\n" +
		"digest: " + lock.Assets["geo"].Digest + "\n" +
		"size: 14 B\n" +
		"path: " + objectPath + "\n"
	if human.status != ExitOK || human.stdout != expectedInfo || human.stderr != "" {
		t.Fatalf("unexpected human info: %#v", human)
	}
	human = run(t, appendArgs(base, "path", "geo@2026.08"))
	if human.status != ExitOK || human.stdout != objectPath+"\n" || human.stderr != "" {
		t.Fatalf("unexpected human path: %#v", human)
	}

	result = runJSON(t, appendArgs(base, "path", "geo@2026.08"))
	assertSuccess(t, result, "path")
	data := result.value["data"].(map[string]any)
	if data["path"] != objectPath {
		t.Fatalf("path result is %#v", data)
	}

	result = runJSON(t, appendArgs(base, "remove", "geo@2026.08"))
	assertSuccess(t, result, "remove")
	manifest, lock = projecttest.Check(t, manifestPath, lockPath)
	if len(manifest.Assets) != 0 || len(lock.Assets) != 0 {
		t.Fatalf("remove left assets: %#v %#v", manifest.Assets, lock.Assets)
	}
	if requests.Load() < 5 {
		t.Fatalf("expected add, lock, and three pull requests, got %d", requests.Load())
	}
}

func TestHumanOutputIsDefaultAndShortJSONAliasWorks(t *testing.T) {
	directory := t.TempDir()
	base := []string{
		"--manifest", filepath.Join(directory, "dac.json"),
		"--lock", filepath.Join(directory, "dac-lock.json"),
	}
	result := run(t, appendArgs(base, "init"))
	if result.status != ExitOK || result.stdout != "Created project files.\n" || result.stderr != "" {
		t.Fatalf("unexpected human init: %#v", result)
	}
	result = run(t, appendArgs(base, "info"))
	if result.status != ExitOK || result.stdout != "No assets.\n" || result.stderr != "" {
		t.Fatalf("unexpected empty info: %#v", result)
	}
	result = run(t, appendArgs(base, "remove", "missing@1"))
	if result.status != ExitFailure || result.stdout != "" || result.stderr != "Error: The project does not have this asset coordinate.\n" {
		t.Fatalf("unexpected human failure: %#v", result)
	}

	jsonResult := runJSONArgs(t, appendArgs(base, "verify", "-j"))
	assertSuccess(t, jsonResult, "verify")
	if jsonResult.stderr != "" {
		t.Fatalf("-j wrote stderr: %q", jsonResult.stderr)
	}
}

func TestInvalidArgumentsUseExitTwoAndOneErrorDocument(t *testing.T) {
	directory := t.TempDir()
	base := []string{
		"--manifest", filepath.Join(directory, "dac.json"),
		"--lock", filepath.Join(directory, "dac-lock.json"),
		"--cache-dir", filepath.Join(directory, "cache"),
	}
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")

	for _, args := range [][]string{
		appendArgs(base, "add", "bad@@1", "--source", "https://example.com/asset"),
		appendArgs(base, "add", "asset@1"),
		appendArgs(base, "add", "asset@1", "--source", "https://example.com/asset", "--timeout", "0"),
		appendArgs(base, "add", "asset@1", "--source", "https://example.com/asset", "--rewrite-config", filepath.Join(directory, "rewrite")),
		appendArgs(base, "info", "asset"),
		appendArgs(base, "info", "asset@1", "other@1"),
		appendArgs(base, "list"),
		appendArgs(base, "rewrite"),
	} {
		result := runJSON(t, args)
		if result.status != ExitUsage {
			t.Fatalf("status=%d stdout=%s stderr=%s", result.status, result.stdout, result.stderr)
		}
		assertError(t, result, "invalid_arguments")
		if result.stderr != "" {
			t.Fatalf("JSON usage error wrote stderr: %q", result.stderr)
		}
	}
}

func TestCommandFailureUsesExitOne(t *testing.T) {
	directory := t.TempDir()
	base := []string{
		"--manifest", filepath.Join(directory, "dac.json"),
		"--lock", filepath.Join(directory, "dac-lock.json"),
		"--cache-dir", filepath.Join(directory, "cache"),
	}
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	result := runJSON(t, appendArgs(base, "remove", "missing@1"))
	if result.status != ExitFailure {
		t.Fatalf("status=%d stdout=%s stderr=%s", result.status, result.stdout, result.stderr)
	}
	assertError(t, result, "asset_unknown")
	if result.stderr != "" {
		t.Fatalf("JSON failure wrote stderr: %q", result.stderr)
	}
}

func TestInitForceReplacesExistingProject(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	base := []string{"--manifest", manifestPath, "--lock", lockPath}
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	if err := os.WriteFile(manifestPath, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	result := runJSON(t, appendArgs(base, "init"))
	if result.status != ExitFailure {
		t.Fatalf("status=%d stdout=%s stderr=%s", result.status, result.stdout, result.stderr)
	}
	assertError(t, result, "manifest_exists")
	assertSuccess(t, runJSON(t, appendArgs(base, "init", "--force")), "init")
	manifest, lock := projecttest.Check(t, manifestPath, lockPath)
	if len(manifest.Assets) != 0 || len(lock.Assets) != 0 {
		t.Fatal("forced init did not create an empty project")
	}
}

func TestOfflineAddWritesOnlyManifest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = writer.Write([]byte("asset"))
	}))
	defer server.Close()

	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	base := []string{"--manifest", manifestPath, "--lock", lockPath}
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	lockBefore := projecttest.MustRead(t, lockPath)

	result := runJSON(t, appendArgs(base, "add", "asset@1", "--source", server.URL, "--offline"))
	assertSuccess(t, result, "add")
	manifest, err := project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Assets["asset"].Version != "1" {
		t.Fatalf("offline add did not update the manifest: %#v", manifest)
	}
	if !bytes.Equal(lockBefore, projecttest.MustRead(t, lockPath)) {
		t.Fatal("offline add changed the lock file")
	}
	if requests.Load() != 0 {
		t.Fatalf("offline add made %d requests", requests.Load())
	}
}

func TestVerifyNeedsNoCacheEnvironment(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	base := []string{"--manifest", manifestPath, "--lock", lockPath}
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	t.Setenv("DAC_CACHE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "relative")
	result := runJSON(t, appendArgs(base, "verify"))
	assertSuccess(t, result, "verify")
}

func TestNetworkFlagsReadEnvironmentSources(t *testing.T) {
	content := []byte("asset")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(content)
	}))
	defer server.Close()
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	cacheRoot := filepath.Join(directory, "environment-cache")
	base := []string{"--manifest", manifestPath, "--lock", lockPath}
	t.Setenv("DAC_CACHE_DIR", cacheRoot)
	t.Setenv("DAC_TIMEOUT", "1s")
	t.Setenv("DAC_RETRIES", "0")
	t.Setenv("DAC_CONCURRENCY", "1")
	t.Setenv("DAC_MAX_SIZE", "1MiB")
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "asset@1", "--source", server.URL, "--progress=false")), "add")
	assertSuccess(t, runJSON(t, appendArgs(base, "lock", "--progress=false")), "lock")
	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	path, err := cache.New(cacheRoot).Path(lock.Assets["asset"].Digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("DAC_CACHE_DIR was not used: %v", err)
	}
}

func TestHelpAndVersionStayOnStderr(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"--version"}, {"--json", "--help"}, {"--json", "--version"}} {
		var stdout bytes.Buffer
		var stderr bytes.Buffer
		if status := Run(context.Background(), args, &stdout, &stderr); status != ExitOK {
			t.Fatalf("args=%v status=%d stderr=%s", args, status, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("args=%v wrote stdout %q", args, stdout.String())
		}
		if stderr.Len() == 0 {
			t.Fatalf("args=%v wrote no help text", args)
		}
	}
}

// runJSON selects JSON mode and decodes the result.
func runJSON(t *testing.T, args []string) invocation {
	t.Helper()
	return runJSONArgs(t, append([]string{"--json"}, args...))
}

// runJSONArgs decodes arguments that already select JSON mode.
func runJSONArgs(t *testing.T, args []string) invocation {
	t.Helper()
	result := run(t, args)
	decoder := json.NewDecoder(strings.NewReader(result.stdout))
	if err := decoder.Decode(&result.value); err != nil {
		t.Fatalf("stdout is not JSON: %v\nstdout: %s\nstderr: %s", err, result.stdout, result.stderr)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("stdout has more than one JSON document: %v\nstdout: %s", err, result.stdout)
	}
	return result
}

// run captures one command invocation without selecting an output mode.
func run(t *testing.T, args []string) invocation {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	status := Run(context.Background(), args, &stdout, &stderr)
	return invocation{status: status, stdout: stdout.String(), stderr: stderr.String()}
}

func appendArgs(base []string, values ...string) []string {
	result := append([]string{}, base...)
	return append(result, values...)
}

func assertSuccess(t *testing.T, result invocation, name string) {
	t.Helper()
	if result.status != ExitOK || result.value["ok"] != true || result.value["command"] != name || result.value["outputVersion"] != float64(1) {
		t.Fatalf("unexpected result: status=%d value=%#v stderr=%s", result.status, result.value, result.stderr)
	}
}

func assertError(t *testing.T, result invocation, code string) {
	t.Helper()
	errorValue, ok := result.value["error"].(map[string]any)
	if !ok || result.value["ok"] != false || errorValue["code"] != code {
		t.Fatalf("unexpected error result: %#v", result.value)
	}
}

func TestParseAgeAcceptsDaysWeeksAndDurations(t *testing.T) {
	for _, testCase := range []struct {
		value    string
		expected time.Duration
	}{
		{"30d", 30 * 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
		{"0d", 0},
		{"1.5d", 36 * time.Hour},
		{"12h", 12 * time.Hour},
		{"90m", 90 * time.Minute},
	} {
		got, err := parseAge(testCase.value)
		if err != nil {
			t.Fatalf("%q: %v", testCase.value, err)
		}
		if got != testCase.expected {
			t.Fatalf("parseAge(%q) = %s, want %s", testCase.value, got, testCase.expected)
		}
	}
}

func TestParseAgeRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "   ", "-1d", "-2h", "d", "seven days", "10x"} {
		if _, err := parseAge(value); err == nil {
			t.Fatalf("%q was accepted", value)
		}
	}
}

// projectFlags sets up a manifest, lock, and cache under one directory and
// returns the global flags that point DAC at them.
type projectFlags struct {
	base         []string
	manifestPath string
	lockPath     string
	cacheRoot    string
}

func newProject(t *testing.T) projectFlags {
	t.Helper()
	directory := t.TempDir()
	flags := projectFlags{
		manifestPath: filepath.Join(directory, "dac.json"),
		lockPath:     filepath.Join(directory, "dac-lock.json"),
		cacheRoot:    filepath.Join(directory, "cache"),
	}
	flags.base = []string{
		"--manifest", flags.manifestPath,
		"--lock", flags.lockPath,
		"--cache-dir", flags.cacheRoot,
	}
	return flags
}

func TestCacheGCRemovesUnusedObjects(t *testing.T) {
	content := []byte("collectable asset")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "geo@1", "--source", server.URL, "--progress=false")), "add")

	// A dry run at zero age reports the object without removing it.
	result := runJSON(t, appendArgs(base, "cache", "gc", "--max-age", "0d", "--dry-run"))
	assertSuccess(t, result, "cache.gc")
	data := result.value["data"].(map[string]any)
	if data["objectCount"].(float64) != 1 || data["dryRun"] != true {
		t.Fatalf("unexpected dry run: %#v", data)
	}
	if human := run(t, appendArgs(base, "path", "geo@1")); human.status != ExitOK {
		t.Fatalf("the dry run removed the object: %#v", human)
	}

	// A long age keeps everything.
	result = runJSON(t, appendArgs(base, "cache", "gc", "--max-age", "30d"))
	assertSuccess(t, result, "cache.gc")
	if result.value["data"].(map[string]any)["objectCount"].(float64) != 0 {
		t.Fatalf("collection removed a fresh object: %#v", result.value)
	}

	// A zero age collects it.
	result = runJSON(t, appendArgs(base, "cache", "gc", "--max-age", "0d"))
	assertSuccess(t, result, "cache.gc")
	if result.value["data"].(map[string]any)["objectCount"].(float64) != 1 {
		t.Fatalf("collection kept the object: %#v", result.value)
	}
	if human := run(t, appendArgs(base, "path", "geo@1")); human.status != ExitFailure {
		t.Fatalf("the object survived collection: %#v", human)
	}
}

func TestCacheGCHumanSummary(t *testing.T) {
	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	human := run(t, appendArgs(base, "cache", "gc"))
	if human.status != ExitOK || human.stdout != "Removed 0 objects (0 B).\n" {
		t.Fatalf("unexpected summary: %#v", human)
	}
	human = run(t, appendArgs(base, "cache", "gc", "--dry-run"))
	if human.status != ExitOK || !strings.HasPrefix(human.stdout, "Would remove") {
		t.Fatalf("unexpected dry run summary: %#v", human)
	}
}

func TestCacheGCRejectsAnInvalidAge(t *testing.T) {
	base := newProject(t).base
	if result := run(t, appendArgs(base, "cache", "gc", "--max-age", "forever")); result.status != ExitUsage {
		t.Fatalf("unexpected status %d: %q", result.status, result.stderr)
	}
}

func TestExportAndOfflineDistDirPull(t *testing.T) {
	content := []byte("portable asset bytes")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	paths := newProject(t)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "geo@1", "--source", server.URL, "--progress=false")), "add")

	bundle := filepath.Join(t.TempDir(), "bundle")
	result := runJSON(t, appendArgs(paths.base, "export", "--dir", bundle))
	assertSuccess(t, result, "export")
	if result.value["data"].(map[string]any)["assetCount"].(float64) != 1 {
		t.Fatalf("unexpected export: %#v", result.value)
	}

	// A second project with a cold cache, no network, and the bundle.
	server.Close()
	offline := newProject(t)
	copyInto(t, paths.manifestPath, offline.manifestPath)
	copyInto(t, paths.lockPath, offline.lockPath)
	result = runJSON(t, appendArgs(offline.base, "pull", "--offline", "--distdir", bundle, "--progress=false", "--concurrency", "1"))
	assertSuccess(t, result, "pull")
	assets := result.value["data"].(map[string]any)["assets"].([]any)
	if assets[0].(map[string]any)["status"] != "distdir" {
		t.Fatalf("pull did not use the bundle: %#v", assets[0])
	}
	if human := run(t, appendArgs(offline.base, "path", "geo@1")); human.status != ExitOK {
		t.Fatalf("the bundle did not populate the cache: %#v", human)
	}
}

func TestInfoCommandCombinesManifestAndRequestState(t *testing.T) {
	paths := newProject(t)
	manifest := project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[string]project.Asset{
			"blocked": {Version: "1", URL: "https://blocked.example.com/one"},
			"kept":    {Version: "2", URL: "https://trusted.example.com/two"},
			"moved":   {Version: "3", URL: "https://vendor.example.com/three"},
		},
	}
	if err := project.Write(paths.manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(filepath.Dir(paths.manifestPath), rewriteConfigName)
	rules := "block *\n" +
		"rewrite ^blocked\\.example\\.com/(.*)$ https://denied.internal/$1\n" +
		"rewrite ^vendor\\.example\\.com/(.*)$ https://mirror.internal/$1\n" +
		"allow trusted.example.com\n" +
		"allow mirror.internal\n"
	if err := os.WriteFile(configPath, []byte(rules), 0o644); err != nil {
		t.Fatal(err)
	}

	human := run(t, appendArgs(paths.base, "info"))
	expected := "blocked@1\n" +
		"source: https://blocked.example.com/one\n" +
		"request: https://denied.internal/one\n" +
		"policy: blocked\n" +
		"lock: missing\n" +
		"cache: unavailable\n\n" +
		"kept@2\n" +
		"source: https://trusted.example.com/two\n" +
		"request: https://trusted.example.com/two\n" +
		"policy: allowed\n" +
		"lock: missing\n" +
		"cache: unavailable\n\n" +
		"moved@3\n" +
		"source: https://vendor.example.com/three\n" +
		"request: https://mirror.internal/three\n" +
		"policy: allowed\n" +
		"lock: missing\n" +
		"cache: unavailable\n"
	if human.status != ExitOK || human.stdout != expected || human.stderr != "" {
		t.Fatalf("unexpected human info result: %#v", human)
	}

	jsonResult := runJSON(t, appendArgs(paths.base, "info"))
	assertSuccess(t, jsonResult, "info")
	data := jsonResult.value["data"].(map[string]any)
	if data["assetCount"] != float64(3) || data["cachedCount"] != float64(0) ||
		data["allowedCount"] != float64(2) || data["blockedCount"] != float64(1) || data["lockStatus"] != "missing" {
		t.Fatalf("unexpected info counts: %#v", data)
	}
	assets := data["assets"].([]any)
	blocked := assets[0].(map[string]any)
	if blocked["name"] != "blocked" || blocked["requestStatus"] != "blocked" || blocked["cacheStatus"] != "unavailable" ||
		blocked["sourceUrl"] != "https://blocked.example.com/one" || blocked["requestUrl"] != "https://denied.internal/one" || blocked["rewritten"] != true {
		t.Fatalf("unexpected blocked asset: %#v", blocked)
	}
	moved := assets[2].(map[string]any)
	if moved["name"] != "moved" || moved["requestStatus"] != "allowed" ||
		moved["sourceUrl"] != "https://vendor.example.com/three" || moved["requestUrl"] != "https://mirror.internal/three" {
		t.Fatalf("unexpected moved asset: %#v", moved)
	}

	single := runJSON(t, appendArgs(paths.base, "info", "moved@3"))
	assertSuccess(t, single, "info")
	singleData := single.value["data"].(map[string]any)
	singleAssets := singleData["assets"].([]any)
	if singleData["assetCount"] != float64(1) || singleData["allowedCount"] != float64(1) ||
		singleData["blockedCount"] != float64(0) || len(singleAssets) != 1 || singleAssets[0].(map[string]any)["name"] != "moved" {
		t.Fatalf("unexpected single info result: %#v", singleData)
	}
	singleHuman := run(t, appendArgs(paths.base, "info", "moved@3"))
	expectedSingle := "moved@3\n" +
		"source: https://vendor.example.com/three\n" +
		"request: https://mirror.internal/three\n" +
		"policy: allowed\n" +
		"lock: missing\n" +
		"cache: unavailable\n"
	if singleHuman.status != ExitOK || singleHuman.stdout != expectedSingle || singleHuman.stderr != "" {
		t.Fatalf("unexpected single human info: %#v", singleHuman)
	}
	unknown := runJSON(t, appendArgs(paths.base, "info", "moved@2"))
	assertError(t, unknown, "asset_unknown")
}

func TestInfoReportsAStaleLockWithoutCacheData(t *testing.T) {
	paths := newProject(t)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "geo@1",
		"--source", "https://example.com/geo", "--offline")), "add")

	result := runJSON(t, appendArgs(paths.base, "info"))
	assertSuccess(t, result, "info")
	data := result.value["data"].(map[string]any)
	asset := data["assets"].([]any)[0].(map[string]any)
	if data["lockStatus"] != "stale" || asset["cacheStatus"] != "unavailable" {
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
	result := runJSON(t, appendArgs(paths.base, "info"))
	assertError(t, result, "lock_invalid")
}

func TestRewriteConfigRedirectsRequests(t *testing.T) {
	content := []byte("mirrored asset bytes")
	var mirrored atomic.Int32
	mirror := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		mirrored.Add(1)
		_, _ = writer.Write(content)
	}))
	defer mirror.Close()

	// The upstream host never answers, so a request that reaches it fails.
	// The replacement carries its own scheme, because one left off keeps the
	// canonical URL's https and the test mirror only speaks HTTP.
	paths := newProject(t)
	configPath := filepath.Join(filepath.Dir(paths.manifestPath), rewriteConfigName)
	rule := "allow_insecure_http\nrewrite ^upstream\\.invalid/(.*)$ " + mirror.URL + "/$1\n"
	if err := os.WriteFile(configPath, []byte(rule), 0o644); err != nil {
		t.Fatal(err)
	}

	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	result := runJSON(t, appendArgs(paths.base, "add", "geo@1",
		"--source", "https://upstream.invalid/geo/database.bin",
		"--progress=false"))
	assertSuccess(t, result, "add")
	if mirrored.Load() != 1 {
		t.Fatalf("the mirror received %d requests, want 1", mirrored.Load())
	}

	// The canonical URL is what gets committed, not the mirror's.
	manifest, lock := projecttest.Check(t, paths.manifestPath, paths.lockPath)
	if manifest.Assets["geo"].URL != "https://upstream.invalid/geo/database.bin" {
		t.Fatalf("the rewrite leaked into the manifest: %q", manifest.Assets["geo"].URL)
	}
	if lock.Assets["geo"].URL != "https://upstream.invalid/geo/database.bin" {
		t.Fatalf("the rewrite leaked into the lock file: %q", lock.Assets["geo"].URL)
	}
}

func TestRewriteConfigAppliesToLockAndPull(t *testing.T) {
	content := []byte("mirrored asset bytes")
	var requests atomic.Int32
	mirror := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = writer.Write(content)
	}))
	defer mirror.Close()

	paths := newProject(t)
	configPath := filepath.Join(filepath.Dir(paths.manifestPath), rewriteConfigName)
	rule := "allow_insecure_http\nrewrite ^upstream\\.invalid/(.*)$ " + mirror.URL + "/$1\n"
	if err := os.WriteFile(configPath, []byte(rule), 0o644); err != nil {
		t.Fatal(err)
	}
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "geo@1",
		"--source", "https://upstream.invalid/geo/database.bin", "--offline")), "add")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "lock",
		"--concurrency", "1", "--progress=false")), "lock")
	if err := os.Remove(objectPathFor(t, paths, "geo")); err != nil {
		t.Fatal(err)
	}
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "pull",
		"--concurrency", "1", "--progress=false")), "pull")
	if requests.Load() != 2 {
		t.Fatalf("the mirror received %d requests, want 2", requests.Load())
	}
}

func TestNoRewriteBypassesAllConfigSources(t *testing.T) {
	content := []byte("canonical asset bytes")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	paths := newProject(t)
	configPath := filepath.Join(filepath.Dir(paths.manifestPath), rewriteConfigName)
	if err := os.WriteFile(configPath, []byte("not-a-directive\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DAC_REWRITE_CONFIG", filepath.Join(t.TempDir(), "absent"))
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "geo@1",
		"--source", server.URL, "--no-rewrite", "--progress=false")), "add")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "lock", "--refresh",
		"--no-rewrite", "--concurrency", "1", "--progress=false")), "lock")
	if err := os.Remove(objectPathFor(t, paths, "geo")); err != nil {
		t.Fatal(err)
	}
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "pull", "--no-rewrite",
		"--concurrency", "1", "--progress=false")), "pull")
	if requests.Load() != 3 {
		t.Fatalf("the canonical server received %d requests, want 3", requests.Load())
	}
}

func TestRewriteConfigEnvironmentOverridesTheProjectConfig(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("asset"))
	}))
	defer server.Close()

	paths := newProject(t)
	projectConfig := filepath.Join(filepath.Dir(paths.manifestPath), rewriteConfigName)
	if err := os.WriteFile(projectConfig, []byte("block *\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	override := filepath.Join(t.TempDir(), "rewrite")
	if err := os.WriteFile(override, []byte("# no rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DAC_REWRITE_CONFIG", override)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	result := runJSON(t, appendArgs(paths.base, "add", "geo@1",
		"--source", server.URL, "--progress=false"))
	assertSuccess(t, result, "add")
}

func TestInvalidProjectRewriteConfigIsAnError(t *testing.T) {
	paths := newProject(t)
	configPath := filepath.Join(filepath.Dir(paths.manifestPath), rewriteConfigName)
	if err := os.WriteFile(configPath, []byte("not-a-directive\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	infoResult := runJSON(t, appendArgs(paths.base, "info"))
	assertError(t, infoResult, "rewrite_config_invalid")
	result := runJSON(t, appendArgs(paths.base, "add", "geo@1",
		"--source", "https://example.com/one", "--progress=false"))
	assertError(t, result, "rewrite_config_invalid")
}

func TestRewriteConfigCanBlockAllHosts(t *testing.T) {
	paths := newProject(t)
	configPath := filepath.Join(filepath.Dir(paths.manifestPath), rewriteConfigName)
	if err := os.WriteFile(configPath, []byte("block *\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	result := runJSON(t, appendArgs(paths.base, "add", "geo@1",
		"--source", "https://files.example.com/one",
		"--progress=false"))
	if result.status != ExitFailure {
		t.Fatalf("unexpected status %d", result.status)
	}
	if code := result.value["error"].(map[string]any)["code"]; code != "url_not_permitted" {
		t.Fatalf("unexpected error code %v", code)
	}
}

func TestMissingRewriteConfigOverrideIsAnError(t *testing.T) {
	t.Setenv("DAC_REWRITE_CONFIG", filepath.Join(t.TempDir(), "absent"))
	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	result := runJSON(t, appendArgs(base, "add", "geo@1",
		"--source", "https://example.com/one",
		"--progress=false"))
	if code := result.value["error"].(map[string]any)["code"]; code != "rewrite_config_missing" {
		t.Fatalf("unexpected error code %v", code)
	}
}

func TestCredentialHelperSuppliesRequestHeaders(t *testing.T) {
	content := []byte("private asset bytes")
	var seen atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen.Store(request.Header.Get("Authorization"))
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	helper := filepath.Join(t.TempDir(), "helper")
	body := "#!/bin/sh\nprintf '{\"headers\":{\"Authorization\":[\"Bearer helper-token\"]}}'\n"
	if err := os.WriteFile(helper, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	result := runJSON(t, appendArgs(base, "add", "geo@1", "--source", server.URL,
		"--credential-helper", helper, "--progress=false"))
	assertSuccess(t, result, "add")
	if got := seen.Load(); got != "Bearer helper-token" {
		t.Fatalf("the server saw Authorization %q", got)
	}
}

func TestFailingCredentialHelperFailsTheCommand(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("asset bytes"))
	}))
	defer server.Close()

	helper := filepath.Join(t.TempDir(), "helper")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	result := runJSON(t, appendArgs(base, "add", "geo@1", "--source", server.URL,
		"--credential-helper", helper, "--progress=false"))
	if result.status != ExitFailure {
		t.Fatalf("unexpected status %d", result.status)
	}
}

func TestAddReportsTheResolvedDigest(t *testing.T) {
	content := []byte("asset bytes")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	human := run(t, appendArgs(base, "add", "geo@1", "--source", server.URL, "--progress=false"))
	if human.status != ExitOK {
		t.Fatalf("unexpected status %d: %q", human.status, human.stderr)
	}
	if !strings.Contains(human.stdout, digest.Bytes(content)) {
		t.Fatalf("the summary hides the resolved digest: %q", human.stdout)
	}
}

func TestPullMismatchReportsTheDigestItReceived(t *testing.T) {
	locked := []byte("asset bytes")
	served := []byte("other bytes")
	var swapped atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if swapped.Load() {
			_, _ = writer.Write(served)
			return
		}
		_, _ = writer.Write(locked)
	}))
	defer server.Close()

	paths := newProject(t)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "geo@1", "--source", server.URL, "--progress=false")), "add")

	if err := os.Remove(objectPathFor(t, paths, "geo")); err != nil {
		t.Fatal(err)
	}
	swapped.Store(true)

	base := paths.base
	result := runJSON(t, appendArgs(base, "pull", "--concurrency", "1", "--progress=false"))
	if result.status != ExitFailure {
		t.Fatalf("unexpected status %d", result.status)
	}
	failure := result.value["error"].(map[string]any)
	details := failure["details"].(map[string]any)
	if details["actualDigest"] != digest.Bytes(served) {
		t.Fatalf("unexpected actualDigest %v", details["actualDigest"])
	}
	if details["expectedDigest"] != digest.Bytes(locked) {
		t.Fatalf("unexpected expectedDigest %v", details["expectedDigest"])
	}
	// The failure has to name the asset, or a multi-asset pull reports a digest
	// pair without saying which asset it belongs to.
	if details["asset"] != "geo" {
		t.Fatalf("the details lost the asset name: %v", details)
	}

	human := run(t, appendArgs(base, "pull", "--concurrency", "1", "--progress=false"))
	if !strings.Contains(human.stderr, digest.Bytes(served)) {
		t.Fatalf("the human error hides the digest DAC received: %q", human.stderr)
	}
}

// objectPathFor returns the cache path of one locked asset.
func objectPathFor(t *testing.T, paths projectFlags, name string) string {
	t.Helper()
	lock, err := project.ReadLock(paths.lockPath)
	if err != nil {
		t.Fatal(err)
	}
	path, err := cache.New(paths.cacheRoot).Path(lock.Assets[name].Digest)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func copyInto(t *testing.T, source, target string) {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
