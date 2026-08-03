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
	"github.com/tom/dac/internal/coord"
	"github.com/tom/dac/internal/digest"
	"github.com/tom/dac/internal/output"
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

	result = runJSON(t, appendArgs(base, "add", "app/geo@2026.08", "--source", server.URL, "--progress=false"))
	assertSuccess(t, result, "add")
	if strings.Contains(result.stdout, "Added") || strings.Contains(result.stdout, "start ") {
		t.Fatalf("human output reached stdout: %q", result.stdout)
	}
	if result.stderr != "" {
		t.Fatalf("JSON add wrote stderr: %q", result.stderr)
	}
	manifest, lock := projecttest.Check(t, manifestPath, lockPath)
	if _, exists := manifest.Assets[coord.MustParse("app/geo@2026.08")]; !exists ||
		lock.Assets[coord.MustParse("app/geo@2026.08")].Digest == "" {
		t.Fatalf("add did not update both files: %#v %#v", manifest, lock)
	}

	result = runJSON(t, appendArgs(base, "info"))
	assertSuccess(t, result, "info")
	infoData := result.value["data"].(map[string]any)
	infoSummary := infoData["summary"].(map[string]any)
	if infoSummary["lockStatus"] != "current" || infoSummary["assetCount"] != float64(1) || infoSummary["cachedCount"] != float64(1) {
		t.Fatalf("unexpected info result: %#v", infoData)
	}
	infoAsset := infoData["assets"].([]any)[0].(map[string]any)
	if infoAsset["coordinate"] != "app/geo@2026.08" || infoAsset["namespace"] != "app" ||
		infoAsset["name"] != "geo" || infoAsset["version"] != "2026.08" || infoAsset["sourceUrl"] != server.URL || infoAsset["requestUrl"] != server.URL ||
		infoAsset["requestStatus"] != "allowed" || infoAsset["rewritten"] != false || infoAsset["cacheStatus"] != "cached" ||
		infoAsset["digest"] != lock.Assets[coord.MustParse("app/geo@2026.08")].Digest || infoAsset["size"] != float64(len(content)) || infoAsset["path"] == "" {
		t.Fatalf("unexpected info asset: %#v", infoAsset)
	}
	result = runJSON(t, appendArgs(base, "verify"))
	assertSuccess(t, result, "verify")
	result = runJSON(t, appendArgs(base, "pull", "--update-lock", "--progress=false", "--concurrency", "1"))
	assertSuccess(t, result, "pull")

	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	store := cache.New(cacheRoot)
	objectPath, err := store.Path(lock.Assets[coord.MustParse("app/geo@2026.08")].Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(objectPath); err != nil {
		t.Fatal(err)
	}
	missingInfo := runJSON(t, appendArgs(base, "info", "app/geo@2026.08"))
	assertSuccess(t, missingInfo, "info")
	missingAsset := missingInfo.value["data"].(map[string]any)["assets"].([]any)[0].(map[string]any)
	if missingAsset["cacheStatus"] != "missing" || missingAsset["digest"] != lock.Assets[coord.MustParse("app/geo@2026.08")].Digest {
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
	if !strings.Contains(humanPull.stderr, "start app/geo@2026.08 ") || !strings.Contains(humanPull.stderr, "done app/geo@2026.08 downloaded") {
		t.Fatalf("human pull progress is incomplete: %q", humanPull.stderr)
	}

	human := run(t, appendArgs(base, "info"))
	expectedInfo := "app/geo@2026.08\n" +
		"source: " + server.URL + "\n" +
		"request: " + server.URL + "\n" +
		"policy: allowed\n" +
		"lock: current\n" +
		"cache: cached\n" +
		"digest: " + lock.Assets[coord.MustParse("app/geo@2026.08")].Digest + "\n" +
		"size: 14 B\n" +
		"path: " + objectPath + "\n"
	if human.status != ExitOK || human.stdout != expectedInfo || human.stderr != "" {
		t.Fatalf("unexpected human info: %#v", human)
	}
	human = run(t, appendArgs(base, "path", "app/geo@2026.08"))
	if human.status != ExitOK || human.stdout != objectPath+"\n" || human.stderr != "" {
		t.Fatalf("unexpected human path: %#v", human)
	}

	result = runJSON(t, appendArgs(base, "path", "app/geo@2026.08"))
	assertSuccess(t, result, "path")
	data := result.value["data"].(map[string]any)
	if data["path"] != objectPath {
		t.Fatalf("path result is %#v", data)
	}

	result = runJSON(t, appendArgs(base, "remove", "app/geo@2026.08"))
	assertSuccess(t, result, "remove")
	manifest, lock = projecttest.Check(t, manifestPath, lockPath)
	if len(manifest.Assets) != 0 || len(lock.Assets) != 0 {
		t.Fatalf("remove left assets: %#v %#v", manifest.Assets, lock.Assets)
	}
	// One request for the add, none for the --update-lock pull because the add
	// already locked the asset, and one for each pull that had to replace the
	// object this test deleted.
	if requests.Load() != 4 {
		t.Fatalf("expected the add and three pull requests, got %d", requests.Load())
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
	result = run(t, appendArgs(base, "remove", "app/missing@1"))
	if result.status != ExitFailure || result.stdout != "" || result.stderr != "Error: The project does not have this asset.\n" {
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
		appendArgs(base, "add", "app/bad@@1", "--source", "https://example.com/asset"),
		appendArgs(base, "add", "app/asset@1"),
		appendArgs(base, "add", "app/asset@1", "--source", "https://example.com/asset", "--timeout", "0"),
		appendArgs(base, "add", "app/asset@1", "--source", "https://example.com/asset", "--rewrite-config", filepath.Join(directory, "rewrite")),
		appendArgs(base, "info", "asset"),
		appendArgs(base, "info", "app/asset@1", "app/other@1"),
		appendArgs(base, "list"),
		appendArgs(base, "rewrite"),
		// Offline mode resolves nothing, so it has nothing to write a lock file
		// from. Both spellings of the update have to say so rather than run and
		// leave the file as they found it.
		appendArgs(base, "pull", "--update-lock", "--offline"),
		appendArgs(base, "pull", "--refresh-lock", "--offline"),
		// Every spelling of a size limit that is not a positive count. Each of
		// these used to leave the guard against a runaway stream switched off
		// without saying so, and the first two are what a shell writes for a
		// variable nobody set.
		appendArgs(base, "pull", "--max-size", "0"),
		appendArgs(base, "pull", "--max-size", ""),
		appendArgs(base, "pull", "--max-size", "99999999TiB"),
		appendArgs(base, "pull", "--max-size", "NaNB"),
		// The lock command is gone rather than hidden or aliased.
		appendArgs(base, "lock"),
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
	result := runJSON(t, appendArgs(base, "remove", "app/missing@1"))
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

	result := runJSON(t, appendArgs(base, "add", "app/asset@1", "--source", server.URL, "--offline"))
	assertSuccess(t, result, "add")
	manifest, err := project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := manifest.Assets[coord.MustParse("app/asset@1")]; !exists {
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
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/asset@1", "--source", server.URL, "--progress=false")), "add")
	assertSuccess(t, runJSON(t, appendArgs(base, "pull", "--update-lock", "--progress=false")), "pull")
	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	path, err := cache.New(cacheRoot).Path(lock.Assets[coord.MustParse("app/asset@1")].Digest)
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
	if result.status != ExitOK || result.value["ok"] != true || result.value["command"] != name || result.value["outputVersion"] != float64(output.Version) {
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
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", "--source", server.URL, "--progress=false")), "add")

	// A dry run at zero age reports the object without removing it.
	result := runJSON(t, appendArgs(base, "cache", "gc", "--max-age", "0d", "--dry-run"))
	assertSuccess(t, result, "cache.gc")
	data := result.value["data"].(map[string]any)
	if data["objectCount"].(float64) != 1 || data["dryRun"] != true {
		t.Fatalf("unexpected dry run: %#v", data)
	}
	if human := run(t, appendArgs(base, "path", "app/geo@1")); human.status != ExitOK {
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
	if human := run(t, appendArgs(base, "path", "app/geo@1")); human.status != ExitFailure {
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

func TestExportAndImportMoveObjectsBetweenCaches(t *testing.T) {
	content := []byte("portable asset bytes")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	paths := newProject(t)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "app/geo@1", "--source", server.URL, "--progress=false")), "add")

	bundle := filepath.Join(t.TempDir(), "cache.tar")
	result := runJSON(t, appendArgs(paths.base, "export", "--file", bundle))
	assertSuccess(t, result, "export")
	if result.value["data"].(map[string]any)["assetCount"].(float64) != 1 {
		t.Fatalf("unexpected export: %#v", result.value)
	}

	// A second project starts with a cold cache and receives the bundle.
	server.Close()
	offline := newProject(t)
	copyInto(t, paths.manifestPath, offline.manifestPath)
	copyInto(t, paths.lockPath, offline.lockPath)
	result = runJSON(t, appendArgs(offline.base, "import", "--file", bundle))
	assertSuccess(t, result, "import")
	data := result.value["data"].(map[string]any)
	if data["itemCount"] != float64(1) || data["objectCount"] != float64(1) || data["byteCount"] != float64(len(content)) {
		t.Fatalf("unexpected import: %#v", data)
	}
	if human := run(t, appendArgs(offline.base, "path", "app/geo@1")); human.status != ExitOK {
		t.Fatalf("the bundle did not populate the cache: %#v", human)
	}
}

func TestInfoCommandCombinesManifestAndRequestState(t *testing.T) {
	paths := newProject(t)
	manifest := project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets: map[coord.Coordinate]project.Asset{
			coord.MustParse("app/blocked@1"): {URL: "https://blocked.example.com/one"},
			coord.MustParse("app/kept@2"):    {URL: "https://trusted.example.com/two"},
			coord.MustParse("app/moved@3"):   {URL: "https://vendor.example.com/three"},
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
	expected := "app/blocked@1\n" +
		"source: https://blocked.example.com/one\n" +
		"request: https://denied.internal/one\n" +
		"policy: blocked\n" +
		"lock: missing\n" +
		"cache: unavailable\n\n" +
		"app/kept@2\n" +
		"source: https://trusted.example.com/two\n" +
		"request: https://trusted.example.com/two\n" +
		"policy: allowed\n" +
		"lock: missing\n" +
		"cache: unavailable\n\n" +
		"app/moved@3\n" +
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
	summary := data["summary"].(map[string]any)
	if summary["assetCount"] != float64(3) || summary["cachedCount"] != float64(0) ||
		summary["corruptCount"] != float64(0) || summary["blockedCount"] != float64(1) || summary["lockStatus"] != "missing" {
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

	single := runJSON(t, appendArgs(paths.base, "info", "app/moved@3"))
	assertSuccess(t, single, "info")
	singleData := single.value["data"].(map[string]any)
	singleAssets := singleData["assets"].([]any)
	singleSummary := singleData["summary"].(map[string]any)
	if singleSummary["assetCount"] != float64(1) || singleSummary["blockedCount"] != float64(0) ||
		len(singleAssets) != 1 || singleAssets[0].(map[string]any)["coordinate"] != "app/moved@3" {
		t.Fatalf("unexpected single info result: %#v", singleData)
	}
	singleHuman := run(t, appendArgs(paths.base, "info", "app/moved@3"))
	expectedSingle := "app/moved@3\n" +
		"source: https://vendor.example.com/three\n" +
		"request: https://mirror.internal/three\n" +
		"policy: allowed\n" +
		"lock: missing\n" +
		"cache: unavailable\n"
	if singleHuman.status != ExitOK || singleHuman.stdout != expectedSingle || singleHuman.stderr != "" {
		t.Fatalf("unexpected single human info: %#v", singleHuman)
	}
	unknown := runJSON(t, appendArgs(paths.base, "info", "app/moved@2"))
	assertError(t, unknown, "asset_unknown")

	// The short form is info's alone: a project can hold several versions of an
	// asset, and listing them is the question this command exists to answer.
	group := runJSON(t, appendArgs(paths.base, "info", "app/moved"))
	assertSuccess(t, group, "info")
	groupAssets := group.value["data"].(map[string]any)["assets"].([]any)
	if len(groupAssets) != 1 || groupAssets[0].(map[string]any)["coordinate"] != "app/moved@3" {
		t.Fatalf("unexpected asset filter result: %#v", groupAssets)
	}
	assertError(t, runJSON(t, appendArgs(paths.base, "info", "app/absent")), "asset_unknown")
}

func TestInfoReportsAStaleLockWithoutCacheData(t *testing.T) {
	paths := newProject(t)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "app/geo@1",
		"--source", "https://example.com/geo", "--offline")), "add")

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
	result := runJSON(t, appendArgs(paths.base, "add", "app/geo@1",
		"--source", "https://upstream.invalid/geo/database.bin",
		"--progress=false"))
	assertSuccess(t, result, "add")
	if mirrored.Load() != 1 {
		t.Fatalf("the mirror received %d requests, want 1", mirrored.Load())
	}

	// The canonical URL is what gets committed, not the mirror's.
	manifest, lock := projecttest.Check(t, paths.manifestPath, paths.lockPath)
	if manifest.Assets[coord.MustParse("app/geo@1")].URL != "https://upstream.invalid/geo/database.bin" {
		t.Fatalf("the rewrite leaked into the manifest: %q", manifest.Assets[coord.MustParse("app/geo@1")].URL)
	}
	if lock.Assets[coord.MustParse("app/geo@1")].URL != "https://upstream.invalid/geo/database.bin" {
		t.Fatalf("the rewrite leaked into the lock file: %q", lock.Assets[coord.MustParse("app/geo@1")].URL)
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
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "app/geo@1",
		"--source", "https://upstream.invalid/geo/database.bin", "--offline")), "add")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "pull", "--update-lock",
		"--concurrency", "1", "--progress=false")), "pull")
	if err := os.Remove(objectPathFor(t, paths, coord.MustParse("app/geo@1"))); err != nil {
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
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "app/geo@1",
		"--source", server.URL, "--no-rewrite", "--progress=false")), "add")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "pull", "--refresh-lock",
		"--no-rewrite", "--concurrency", "1", "--progress=false")), "pull")
	if err := os.Remove(objectPathFor(t, paths, coord.MustParse("app/geo@1"))); err != nil {
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
	result := runJSON(t, appendArgs(paths.base, "add", "app/geo@1",
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
	result := runJSON(t, appendArgs(paths.base, "add", "app/geo@1",
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
	result := runJSON(t, appendArgs(paths.base, "add", "app/geo@1",
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
	result := runJSON(t, appendArgs(base, "add", "app/geo@1",
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
	result := runJSON(t, appendArgs(base, "add", "app/geo@1", "--source", server.URL,
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
	result := runJSON(t, appendArgs(base, "add", "app/geo@1", "--source", server.URL,
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
	human := run(t, appendArgs(base, "add", "app/geo@1", "--source", server.URL, "--progress=false"))
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
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "app/geo@1", "--source", server.URL, "--progress=false")), "add")

	if err := os.Remove(objectPathFor(t, paths, coord.MustParse("app/geo@1"))); err != nil {
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
	if details["asset"] != "app/geo@1" {
		t.Fatalf("the details lost the asset name: %v", details)
	}

	human := run(t, appendArgs(base, "pull", "--concurrency", "1", "--progress=false"))
	if !strings.Contains(human.stderr, digest.Bytes(served)) {
		t.Fatalf("the human error hides the digest DAC received: %q", human.stderr)
	}
}

// objectPathFor returns the cache path of one locked asset.
func objectPathFor(t *testing.T, paths projectFlags, name coord.Coordinate) string {
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

// corruptObject rewrites a cache object in place, keeping its size, which is
// the damage that used to pass every check DAC made.
func corruptObject(t *testing.T, cacheRoot, value string) {
	t.Helper()
	path := filepath.Join(cacheRoot, "blobs", "sha256", strings.TrimPrefix(value, digest.Prefix))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.ToUpper(original), 0o444); err != nil {
		t.Fatal(err)
	}
	if len(bytes.ToUpper(original)) != len(original) {
		t.Fatal("the test needs damage that preserves the size")
	}
}

// TestCorruptCacheObjectIsCaughtEndToEnd walks the failure the store used to
// hand back without comment: a cache object edited in place, keeping its size.
func TestCorruptCacheObjectIsCaughtEndToEnd(t *testing.T) {
	content := []byte("mini dac asset")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	directory := t.TempDir()
	cacheRoot := filepath.Join(directory, "cache")
	base := []string{
		"--manifest", filepath.Join(directory, "dac.json"),
		"--lock", filepath.Join(directory, "dac-lock.json"),
		"--cache-dir", cacheRoot,
	}
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", "--source", server.URL, "--progress=false")), "add")
	corruptObject(t, cacheRoot, digest.Bytes(content))

	// path used to return this object, and every script downstream would have
	// read the wrong bytes without ever being told.
	assertError(t, runJSON(t, appendArgs(base, "path", "app/geo@1")), "cache_object_corrupt")
	assertError(t, runJSON(t, appendArgs(base, "export", "--file", filepath.Join(directory, "bundle.tar"))), "cache_object_corrupt")
	assertError(t, runJSON(t, appendArgs(base, "cache", "verify")), "cache_object_corrupt")

	info := runJSON(t, appendArgs(base, "info"))
	assertSuccess(t, info, "info")
	assets := info.value["data"].(map[string]any)["assets"].([]any)
	if assets[0].(map[string]any)["cacheStatus"] != "corrupt" {
		t.Fatalf("info did not report the damage: %#v", assets[0])
	}

	// pull replaces the object, and the whole project comes back clean.
	assertSuccess(t, runJSON(t, appendArgs(base, "pull", "--progress=false")), "pull")
	assertSuccess(t, runJSON(t, appendArgs(base, "path", "app/geo@1")), "path")
	verified := runJSON(t, appendArgs(base, "cache", "verify"))
	assertSuccess(t, verified, "cache.verify")
	if verified.value["data"].(map[string]any)["corruptCount"] != float64(0) {
		t.Fatalf("the cache did not come back clean: %#v", verified.value["data"])
	}
}

func TestCacheVerifyRepairRemovesTheDamage(t *testing.T) {
	content := []byte("mini dac asset")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	directory := t.TempDir()
	cacheRoot := filepath.Join(directory, "cache")
	base := []string{
		"--manifest", filepath.Join(directory, "dac.json"),
		"--lock", filepath.Join(directory, "dac-lock.json"),
		"--cache-dir", cacheRoot,
	}
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", "--source", server.URL, "--progress=false")), "add")
	corruptObject(t, cacheRoot, digest.Bytes(content))

	repaired := runJSON(t, appendArgs(base, "cache", "verify", "--repair"))
	assertSuccess(t, repaired, "cache.verify")
	data := repaired.value["data"].(map[string]any)
	if data["corruptCount"] != float64(1) || data["repaired"] != float64(1) {
		t.Fatalf("unexpected repair result: %#v", data)
	}
	path := filepath.Join(cacheRoot, "blobs", "sha256", strings.TrimPrefix(digest.Bytes(content), digest.Prefix))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("--repair left the object in place: %v", err)
	}
}

func TestCacheDirReportsTheResolvedRoot(t *testing.T) {
	directory := t.TempDir()
	cacheRoot := filepath.Join(directory, "cache")
	result := runJSON(t, []string{"--cache-dir", cacheRoot, "cache", "dir"})
	assertSuccess(t, result, "cache.dir")
	if result.value["data"].(map[string]any)["path"] != cacheRoot {
		t.Fatalf("unexpected cache root: %#v", result.value["data"])
	}
	// The human form is the path alone, so a script can use it directly.
	plain := run(t, []string{"--cache-dir", cacheRoot, "cache", "dir"})
	if strings.TrimSpace(plain.stdout) != cacheRoot {
		t.Fatalf("unexpected human output: %q", plain.stdout)
	}
}

// TestJSONErrorsCarryTheirCause covers the failure an operator actually meets
// in CI: a pull that cannot reach its origin. The stable code says what kind of
// failure it was, and nothing else in the document used to say which URL or
// what the server answered.
func TestJSONErrorsCarryTheirCause(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	directory := t.TempDir()
	base := []string{
		"--manifest", filepath.Join(directory, "dac.json"),
		"--lock", filepath.Join(directory, "dac-lock.json"),
		"--cache-dir", filepath.Join(directory, "cache"),
	}
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")

	result := runJSON(t, appendArgs(base, "add", "app/geo@1", "--source", server.URL, "--retries", "0", "--progress=false"))
	assertError(t, result, "network_error")
	errorValue := result.value["error"].(map[string]any)
	cause, _ := errorValue["cause"].(string)
	if !strings.Contains(cause, "404") {
		t.Fatalf("the cause did not describe the failure: %#v", errorValue)
	}
	details := errorValue["details"].(map[string]any)
	if details["status"] != float64(http.StatusNotFound) || details["url"] != server.URL {
		t.Fatalf("the details did not name the request: %#v", details)
	}
}

func TestHumanErrorsIncludeTheirCause(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	directory := t.TempDir()
	base := []string{
		"--manifest", filepath.Join(directory, "dac.json"),
		"--lock", filepath.Join(directory, "dac-lock.json"),
		"--cache-dir", filepath.Join(directory, "cache"),
	}
	if status := run(t, appendArgs(base, "init")).status; status != ExitOK {
		t.Fatalf("init failed with status %d", status)
	}
	result := run(t, appendArgs(base, "add", "app/geo@1", "--source", server.URL, "--retries", "0", "--progress=false"))
	if result.status != ExitFailure || !strings.Contains(result.stderr, "404") {
		t.Fatalf("the human error hid its cause: status=%d stderr=%q", result.status, result.stderr)
	}
	// The content-check message used to print its detail twice, once inside the
	// message and once again as the cause.
	if strings.Count(result.stderr, "404") != 1 {
		t.Fatalf("the cause was printed more than once: %q", result.stderr)
	}
}

// The path a project takes when its lock file is not committed, or when someone
// edits the manifest by hand: a plain pull says what to run, and that command
// writes the file and names what it locked.
func TestPullUpdateLockWritesAMissingLockFile(t *testing.T) {
	content := []byte("asset bytes")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	base := []string{"--manifest", manifestPath, "--lock", lockPath, "--cache-dir", filepath.Join(directory, "cache")}
	manifest := project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets:        map[coord.Coordinate]project.Asset{coord.MustParse("app/geo@1"): {URL: server.URL}},
	}
	data, err := project.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	assertError(t, runJSON(t, appendArgs(base, "pull", "--progress=false")), "lock_missing")
	human := run(t, appendArgs(base, "pull", "--update-lock", "--progress=false"))
	if human.status != ExitOK || human.stdout != "Pulled 1 asset. Locked app/geo@1.\n" {
		t.Fatalf("unexpected human pull: %#v", human)
	}
	projecttest.Check(t, manifestPath, lockPath)
}

func TestVerifyRefreshFailsOnDrift(t *testing.T) {
	var body atomic.Value
	body.Store([]byte("first bytes"))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(body.Load().([]byte))
	}))
	defer server.Close()

	directory := t.TempDir()
	lockPath := filepath.Join(directory, "dac-lock.json")
	base := []string{
		"--manifest", filepath.Join(directory, "dac.json"),
		"--lock", lockPath,
		"--cache-dir", filepath.Join(directory, "cache"),
	}
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", "--source", server.URL, "--progress=false")), "add")
	assertSuccess(t, runJSON(t, appendArgs(base, "verify", "--refresh", "--progress=false")), "verify")

	body.Store([]byte("moved bytes"))
	before := projecttest.MustRead(t, lockPath)
	result := runJSON(t, appendArgs(base, "verify", "--refresh", "--progress=false"))
	assertError(t, result, "lock_drift")
	if !bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("--refresh rewrote the lock file")
	}
}

// TestMaxSizeBoundsADownloadAndOnlyNoneRemovesTheBound checks the guard from
// both ends. It has to stop a response nothing else bounds, and the only way to
// switch it off has to be a word an operator typed on purpose.
func TestMaxSizeBoundsADownloadAndOnlyNoneRemovesTheBound(t *testing.T) {
	content := bytes.Repeat([]byte("asset"), 1024)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	directory := t.TempDir()
	base := []string{
		"--manifest", filepath.Join(directory, "dac.json"),
		"--lock", filepath.Join(directory, "dac-lock.json"),
		"--cache-dir", filepath.Join(directory, "cache"),
	}
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertError(t, runJSON(t, appendArgs(base, "add", "app/geo@1",
		"--source", server.URL, "--max-size", "1KiB", "--progress=false")), "asset_too_large")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1",
		"--source", server.URL, "--max-size", noSizeLimit, "--progress=false")), "add")
}

// TestVerifyRefreshReportsDriftForAPinnedAsset covers the asset the drift check
// was least able to report on. A pinned asset used to be resolved against its
// publisher digest even here, so a moved origin failed the download with
// content_mismatch before anything compared it to the lock -- and lock_drift,
// the code a scheduled job branches on, never appeared for the assets somebody
// cared enough about to pin. Pull keeps enforcing the pin, because pull writes.
func TestVerifyRefreshReportsDriftForAPinnedAsset(t *testing.T) {
	var body atomic.Value
	body.Store([]byte("first bytes"))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(body.Load().([]byte))
	}))
	defer server.Close()

	directory := t.TempDir()
	lockPath := filepath.Join(directory, "dac-lock.json")
	base := []string{
		"--manifest", filepath.Join(directory, "dac.json"),
		"--lock", lockPath,
		"--cache-dir", filepath.Join(directory, "cache"),
	}
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1",
		"--source", server.URL, "--pin", "--progress=false")), "add")
	assertSuccess(t, runJSON(t, appendArgs(base, "verify", "--refresh", "--progress=false")), "verify")

	body.Store([]byte("moved bytes"))
	before := projecttest.MustRead(t, lockPath)
	result := runJSON(t, appendArgs(base, "verify", "--refresh", "--progress=false"))
	assertError(t, result, "lock_drift")
	if !bytes.Contains([]byte(result.stdout), []byte(`"app/geo@1"`)) {
		t.Fatalf("lock_drift did not name the drifted asset: %s", result.stdout)
	}
	if !bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("--refresh rewrote the lock file")
	}
	// The same origin against the same pin, from the command that writes: a pin
	// is a rule there, and bytes that fail it must not reach the lock file.
	assertError(t, runJSON(t, appendArgs(base, "pull", "--refresh-lock", "--progress=false")), "content_mismatch")
	if !bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("a failed --refresh-lock rewrote the lock file")
	}
}

func TestAddPinWritesTheIntegrityValue(t *testing.T) {
	content := []byte("mini dac asset")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	base := []string{"--manifest", manifestPath, "--lock", lockPath, "--cache-dir", filepath.Join(directory, "cache")}
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", "--source", server.URL, "--pin", "--progress=false")), "add")

	manifest, _ := projecttest.Check(t, manifestPath, lockPath)
	if manifest.Assets[coord.MustParse("app/geo@1")].Integrity != digest.Bytes(content) {
		t.Fatalf("add --pin did not write the integrity value: %#v", manifest.Assets[coord.MustParse("app/geo@1")])
	}
}
