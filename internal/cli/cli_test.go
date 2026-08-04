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

// TestMain points the config search path at an empty directory.
//
// DAC reads a config file from the XDG base directories, so without this every
// test in this package would read whatever the machine running it has
// installed, and a developer with a config would get different results from
// CI. Setting it once for the process rather than per test keeps t.Setenv out
// of tests that would otherwise be free to run in parallel.
func TestMain(main *testing.M) {
	root, err := os.MkdirTemp("", "dac-cli-test")
	if err != nil {
		panic(err)
	}
	for name, value := range map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"XDG_CONFIG_DIRS": filepath.Join(root, "system"),
		"HOME":            root,
	} {
		if err := os.Setenv(name, value); err != nil {
			panic(err)
		}
	}
	if err := os.Unsetenv("DAC_CONFIG"); err != nil {
		panic(err)
	}
	code := main.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}

func TestCommandLifecycle(t *testing.T) {
	content := []byte("mini dac asset")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.Header().Set("ETag", "\"asset-1\"")
		// The server URL carries no path, so the header is the only thing that
		// names this asset. It covers the whole chain from response to lock
		// entry to reported field.
		writer.Header().Set("Content-Disposition", `attachment; filename="geo.bin"`)
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

	result = runJSON(t, appendArgs(base, "add", "app/geo@2026.08", server.URL, "--no-progress"))
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
	if name := lock.Assets[coord.MustParse("app/geo@2026.08")].Filename; name != "geo.bin" {
		t.Fatalf("the lock recorded the file name %q", name)
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
		infoAsset["filename"] != "geo.bin" ||
		infoAsset["digest"] != lock.Assets[coord.MustParse("app/geo@2026.08")].Digest || infoAsset["size"] != float64(len(content)) || infoAsset["path"] == "" {
		t.Fatalf("unexpected info asset: %#v", infoAsset)
	}
	result = runJSON(t, appendArgs(base, "verify"))
	assertSuccess(t, result, "verify")
	result = runJSON(t, appendArgs(base, "lock", "--no-progress", "--concurrency", "1"))
	assertSuccess(t, result, "lock")
	result = runJSON(t, appendArgs(base, "pull", "--no-progress", "--concurrency", "1"))
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
		"filename: geo.bin\n" +
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
	// One request for the add, none for the lock or the pull that follows it
	// because the add already locked and cached the asset, and one for each pull
	// that had to replace the object this test deleted.
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
		appendArgs(base, "add", "app/bad@@1", "https://example.com/asset"),
		// The source is an argument rather than a flag, so an add that names no
		// source is an add with the wrong number of arguments.
		appendArgs(base, "add", "app/asset@1"),
		appendArgs(base, "add"),
		appendArgs(base, "add", "app/asset@1", "https://example.com/asset", "https://example.com/other"),
		appendArgs(base, "add", "app/asset@1", "  "),
		// A version is not optional where a command writes: an add that picked
		// one for itself would rewrite whichever version it guessed.
		appendArgs(base, "add", "app/asset", "https://example.com/asset"),
		appendArgs(base, "add", "app/asset@1", "https://example.com/asset", "--rewrite-config", filepath.Join(directory, "rewrite")),
		appendArgs(base, "info", "asset"),
		appendArgs(base, "info", "app/asset@1", "app/other@1"),
		appendArgs(base, "list"),
		appendArgs(base, "rewrite"),
		// Offline mode resolves nothing, so it has nothing to write a lock file
		// from. The lock command takes no --offline at all rather than accepting
		// one and leaving the file as it found it.
		appendArgs(base, "lock", "--offline"),
		appendArgs(base, "lock", "--refresh", "--offline"),
		// The transfer tuning options moved into the config file, so the
		// command line no longer answers to them at all.
		appendArgs(base, "pull", "--max-size", "1GiB"),
		appendArgs(base, "pull", "--timeout", "1m"),
		appendArgs(base, "pull", "--retries", "1"),
		appendArgs(base, "pull", "--download-parts", "2"),
		appendArgs(base, "pull", "--credential-helper", "helper"),
		appendArgs(base, "pull", "--distdir", directory),
		appendArgs(base, "pull", "--progress=false"),
		// Every lock operation lives on the lock command, so pull no longer
		// answers to the flags that used to spell them.
		appendArgs(base, "pull", "--update-lock"),
		appendArgs(base, "pull", "--refresh-lock"),
		appendArgs(base, "pull", "--rebind"),
		// Pull is the command that installs, so the lock command does not take
		// the flags that only mean something to an installation.
		appendArgs(base, "lock", "--distdir", directory),
	} {
		result := runJSON(t, args)
		if result.status != ExitUsage {
			t.Fatalf("args=%v status=%d stdout=%s stderr=%s", args, result.status, result.stdout, result.stderr)
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

	result := runJSON(t, appendArgs(base, "add", "app/asset@1", server.URL, "--offline"))
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
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/asset@1", server.URL, "--no-progress")), "add")
	assertSuccess(t, runJSON(t, appendArgs(base, "lock", "--no-progress")), "lock")
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

// configFlags writes a config file and returns the flags that select it.
//
// The transfer settings live in a file now, so a test that needs one goes
// through the same --config path a deployment does rather than through a
// command line that no longer accepts them.
func configFlags(t *testing.T, text string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"--config", path}
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

// TestPathAcceptsAnAssetWithoutItsVersion covers the shorter form and the one
// case it must refuse. A project that carries one version of a thing has
// nothing to choose between, and repeating a version the manifest already holds
// is noise inside a shell substitution. A project that carries two does have
// something to choose between, and DAC does not order versions -- it has no
// idea whether "17" follows "11" -- so there is no latest to fall back on and
// picking either one would be inventing an answer.
func TestPathAcceptsAnAssetWithoutItsVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "bytes for "+request.URL.Path)
	}))
	defer server.Close()

	paths := newProject(t)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "java/sdk@11",
		server.URL+"/jdk11.tar.gz", "--no-progress")), "add")

	objectPath := objectPathFor(t, paths, coord.MustParse("java/sdk@11"))
	bare := run(t, appendArgs(paths.base, "path", "java/sdk"))
	if bare.status != ExitOK || bare.stdout != objectPath+"\n" {
		t.Fatalf("path without a version returned %#v", bare)
	}
	versioned := run(t, appendArgs(paths.base, "path", "java/sdk@11"))
	if versioned.stdout != bare.stdout {
		t.Fatalf("the two spellings disagree: %q and %q", versioned.stdout, bare.stdout)
	}
	// The result names the version it resolved to, so a caller that left it off
	// can still tell which asset answered.
	result := runJSON(t, appendArgs(paths.base, "path", "java/sdk"))
	assertSuccess(t, result, "path")
	if data := result.value["data"].(map[string]any); data["coordinate"] != "java/sdk@11" || data["version"] != "11" {
		t.Fatalf("path did not report the version it resolved: %#v", data)
	}

	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "java/sdk@17",
		server.URL+"/jdk17.tar.gz", "--no-progress")), "add")

	ambiguous := runJSON(t, appendArgs(paths.base, "path", "java/sdk"))
	assertError(t, ambiguous, "asset_ambiguous")
	details := ambiguous.value["error"].(map[string]any)["details"].(map[string]any)
	versions := details["versions"].([]any)
	if details["asset"] != "java/sdk" || len(versions) != 2 || versions[0] != "11" || versions[1] != "17" {
		t.Fatalf("the refusal did not list the versions to choose from: %#v", details)
	}
	// Naming the version is what resolves it, and both are still reachable.
	for _, version := range []string{"11", "17"} {
		named := run(t, appendArgs(paths.base, "path", "java/sdk@"+version))
		if named.status != ExitOK || named.stdout == "" {
			t.Fatalf("path java/sdk@%s returned %#v", version, named)
		}
	}
	// An asset the project does not have is a different answer from one it has
	// too many of, and it keeps the code it always had.
	assertError(t, runJSON(t, appendArgs(paths.base, "path", "java/nope")), "asset_unknown")
	assertError(t, runJSON(t, appendArgs(paths.base, "path", "not-a-coordinate")), "invalid_arguments")
	assertError(t, runJSON(t, appendArgs(paths.base, "path")), "invalid_arguments")
}

func TestCacheGCRemovesUnusedObjects(t *testing.T) {
	content := []byte("collectable asset")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--no-progress")), "add")

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
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "app/geo@1", server.URL, "--no-progress")), "add")

	bundle := filepath.Join(t.TempDir(), "cache.tar")
	result := runJSON(t, appendArgs(paths.base, "export", bundle))
	assertSuccess(t, result, "export")
	if result.value["data"].(map[string]any)["assetCount"].(float64) != 1 {
		t.Fatalf("unexpected export: %#v", result.value)
	}

	// A second project starts with a cold cache and receives the bundle.
	server.Close()
	offline := newProject(t)
	copyInto(t, paths.manifestPath, offline.manifestPath)
	copyInto(t, paths.lockPath, offline.lockPath)
	result = runJSON(t, appendArgs(offline.base, "import", bundle))
	assertSuccess(t, result, "import")
	data := result.value["data"].(map[string]any)
	if data["itemCount"] != float64(1) || data["objectCount"] != float64(1) || data["byteCount"] != float64(len(content)) {
		t.Fatalf("unexpected import: %#v", data)
	}
	if human := run(t, appendArgs(offline.base, "path", "app/geo@1")); human.status != ExitOK {
		t.Fatalf("the bundle did not populate the cache: %#v", human)
	}

	// The bundle path is the argument both halves take, and neither has a
	// default to fall back on. Leaving it off, naming two, or handing over the
	// empty string a shell makes from a variable nobody set are all mistakes
	// rather than requests to guess at a file name.
	assertError(t, runJSON(t, appendArgs(paths.base, "export")), "invalid_arguments")
	assertError(t, runJSON(t, appendArgs(paths.base, "export", bundle, "extra")), "invalid_arguments")
	assertError(t, runJSON(t, appendArgs(paths.base, "export", "  ")), "invalid_arguments")
	assertError(t, runJSON(t, appendArgs(offline.base, "import")), "invalid_arguments")
	assertError(t, runJSON(t, appendArgs(offline.base, "import", bundle, "extra")), "invalid_arguments")
}

// TestPackAndUnpackDefaultToOneArchiveAndTheWorkingDirectory covers the two
// defaults that make the pair usable without arguments: the archive both halves
// agree on, and the directory unpack materializes into. Neither is a flag,
// because a dacpack is a build output a project makes one of rather than a
// thing being moved to a destination somebody had to choose.
func TestPackAndUnpackDefaultToOneArchiveAndTheWorkingDirectory(t *testing.T) {
	content := []byte("materialized asset bytes")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	paths := newProject(t)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "app/geo@1",
		server.URL+"/geo.bin", "--no-progress")), "add")

	// Both defaults are relative, so they land where the command was run, which
	// is what a script controls by choosing where it runs. t.Chdir restores the
	// directory afterwards and refuses to run in a parallel test, which is the
	// part that would otherwise make this unsafe for its neighbours.
	working := t.TempDir()
	t.Chdir(working)

	result := runJSON(t, appendArgs(paths.base, "pack"))
	assertSuccess(t, result, "pack")
	archive := filepath.Join(working, "dac.dacpack")
	if written := result.value["data"].(map[string]any)["pack"]; written != archive {
		t.Fatalf("pack wrote %v, want the default beside the caller: %s", written, archive)
	}

	// Unpack reads no project files and needs no cache, so nothing here points
	// it at either. It answers with files on disk rather than a warm cache.
	result = runJSON(t, []string{"unpack"})
	assertSuccess(t, result, "unpack")
	data := result.value["data"].(map[string]any)
	target := filepath.Join(working, "assets", "app", "geo", "1", "geo.bin")
	if data["pack"] != archive || data["directory"] != working ||
		data["itemCount"] != float64(1) || data["fileCount"] != float64(1) ||
		data["byteCount"] != float64(len(content)) {
		t.Fatalf("unexpected unpack: %#v", data)
	}
	files := data["files"].([]any)
	if len(files) != 1 || files[0].(map[string]any)["path"] != target ||
		files[0].(map[string]any)["coordinate"] != "app/geo@1" {
		t.Fatalf("unexpected file report: %#v", files)
	}
	written, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(written, content) {
		t.Fatalf("the unpack wrote %q to %s: %v", written, target, err)
	}

	// Unpacking again would replace what the first one wrote, so it stops.
	assertError(t, runJSON(t, []string{"unpack"}), "unpack_destination_occupied")
	assertSuccess(t, runJSON(t, []string{"unpack", "--force"}), "unpack")

	// Both positionals, named explicitly.
	named := filepath.Join(t.TempDir(), "named.dacpack")
	elsewhere := t.TempDir()
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "pack", named)), "pack")
	assertSuccess(t, runJSON(t, []string{"unpack", named, elsewhere}), "unpack")
	if _, err := os.Stat(filepath.Join(elsewhere, "assets", "app", "geo", "1", "geo.bin")); err != nil {
		t.Fatalf("the named destination is empty: %v", err)
	}
	// Too many arguments is a mistake rather than something to guess at.
	assertError(t, runJSON(t, []string{"unpack", named, elsewhere, "extra"}), "invalid_arguments")
	assertError(t, runJSON(t, appendArgs(paths.base, "pack", named, "extra")), "invalid_arguments")
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
		"https://upstream.invalid/geo/database.bin",
		"--no-progress"))
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
		"https://upstream.invalid/geo/database.bin", "--offline")), "add")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "lock",
		"--concurrency", "1", "--no-progress")), "lock")
	if err := os.Remove(objectPathFor(t, paths, coord.MustParse("app/geo@1"))); err != nil {
		t.Fatal(err)
	}
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "pull",
		"--concurrency", "1", "--no-progress")), "pull")
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
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "app/geo@1",
		server.URL, "--no-rewrite", "--no-progress")), "add")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "lock", "--refresh",
		"--no-rewrite", "--concurrency", "1", "--no-progress")), "lock")
	if err := os.Remove(objectPathFor(t, paths, coord.MustParse("app/geo@1"))); err != nil {
		t.Fatal(err)
	}
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "pull", "--no-rewrite",
		"--concurrency", "1", "--no-progress")), "pull")
	if requests.Load() != 3 {
		t.Fatalf("the canonical server received %d requests, want 3", requests.Load())
	}
}

// Host policy belongs to the site, so the config file is where it lives. A
// project that carries its own dac-rewrite.cfg is saying something about itself
// rather than adding to what the site said, so it replaces the config file's
// rules outright instead of merging with them.
func TestProjectRewriteConfigOverridesTheConfigFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("asset"))
	}))
	defer server.Close()

	paths := newProject(t)
	base := append(configFlags(t, "[hosts]\nblock = [\"*\"]\n"), paths.base...)
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")

	// The config file blocks everything, so the add is refused.
	blocked := runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--no-progress"))
	assertError(t, blocked, "url_not_permitted")

	// A project config that permits the host wins over it.
	projectConfig := filepath.Join(filepath.Dir(paths.manifestPath), rewriteConfigName)
	if err := os.WriteFile(projectConfig, []byte("# no rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--no-progress")), "add")
}

// The config file is the site-wide home for rewriting, which is the scope the
// feature was always for: a site that proxies its downloads should not have to
// edit every repository that uses them.
func TestConfigFileRewritesRequestURLs(t *testing.T) {
	var requests atomic.Int32
	mirror := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = writer.Write([]byte("mirrored asset"))
	}))
	defer mirror.Close()

	paths := newProject(t)
	settings := "[[rewrite]]\npattern = '^canonical\\.example\\.com/(.*)$'\nreplacement = \"" + mirror.URL + "/$1\"\n"
	base := append(configFlags(t, settings), paths.base...)
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1",
		"https://canonical.example.com/geo/db.bin", "--no-progress")), "add")
	if requests.Load() != 1 {
		t.Fatalf("the mirror received %d requests, want 1", requests.Load())
	}

	// The manifest and lock keep the canonical URL, and info reports both.
	result := runJSON(t, appendArgs(base, "info"))
	assertSuccess(t, result, "info")
	asset := result.value["data"].(map[string]any)["assets"].([]any)[0].(map[string]any)
	if asset["sourceUrl"] != "https://canonical.example.com/geo/db.bin" {
		t.Fatalf("the source URL was rewritten: %v", asset["sourceUrl"])
	}
	if request, _ := asset["requestUrl"].(string); !strings.HasPrefix(request, mirror.URL) {
		t.Fatalf("the request URL was not rewritten: %v", asset["requestUrl"])
	}
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
		"https://example.com/one", "--no-progress"))
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
		"https://files.example.com/one",
		"--no-progress"))
	if result.status != ExitFailure {
		t.Fatalf("unexpected status %d", result.status)
	}
	if code := result.value["error"].(map[string]any)["code"]; code != "url_not_permitted" {
		t.Fatalf("unexpected error code %v", code)
	}
}

// A search path that finds nothing means a machine that wants the defaults. A
// --config naming a file that is not there is a deployment that thinks it
// configured something, so the two answer differently.
func TestMissingConfigFileIsAnError(t *testing.T) {
	base := append([]string{"--config", filepath.Join(t.TempDir(), "absent.toml")}, newProject(t).base...)
	result := runJSON(t, appendArgs(base, "pull"))
	assertError(t, result, "config_invalid")
	if cause, _ := result.value["error"].(map[string]any)["cause"].(string); !strings.Contains(cause, "absent.toml") {
		t.Fatalf("the cause did not name the file: %v", result.value)
	}
}

func TestInvalidConfigFileIsAnError(t *testing.T) {
	base := append(configFlags(t, "[transfer]\ntimeuot = \"5m\"\n"), newProject(t).base...)
	result := runJSON(t, appendArgs(base, "pull"))
	assertError(t, result, "config_invalid")
}

// Every option version 7 took off the command line says where it went. "That is
// not a flag" would be true and useless to somebody with it in a script.
func TestRetiredFlagsNameTheirConfigKey(t *testing.T) {
	base := newProject(t).base
	for flag, want := range map[string]string{
		"--timeout":           "transfer.timeout",
		"--retries":           "transfer.retries",
		"--download-parts":    "transfer.download-parts",
		"--max-size":          "transfer.max-size",
		"--credential-helper": "credentials",
		"--progress":          "transfer.progress",
	} {
		result := run(t, appendArgs(base, "pull", flag, "1"))
		if result.status != ExitUsage {
			t.Errorf("%s: status = %d, want %d", flag, result.status, ExitUsage)
			continue
		}
		if !strings.Contains(result.stderr, want) {
			t.Errorf("%s: stderr does not name %s: %q", flag, want, result.stderr)
		}
	}
	// A surviving flag whose name starts the same way must not be mistaken for
	// a retired one.
	if result := run(t, appendArgs(base, "cache", "gc", "--max-age", "1s")); result.status != ExitOK {
		t.Errorf("cache gc --max-age: status = %d, stderr = %q", result.status, result.stderr)
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

	base := append(configFlags(t, "[credentials]\ndefault = \""+helper+"\"\n"), newProject(t).base...)
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	result := runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--no-progress"))
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
	base := append(configFlags(t, "[credentials]\ndefault = \""+helper+"\"\n"), newProject(t).base...)
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	result := runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--no-progress"))
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
	human := run(t, appendArgs(base, "add", "app/geo@1", server.URL, "--no-progress"))
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
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "app/geo@1", server.URL, "--no-progress")), "add")

	if err := os.Remove(objectPathFor(t, paths, coord.MustParse("app/geo@1"))); err != nil {
		t.Fatal(err)
	}
	swapped.Store(true)

	base := paths.base
	result := runJSON(t, appendArgs(base, "pull", "--concurrency", "1", "--no-progress"))
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

	human := run(t, appendArgs(base, "pull", "--concurrency", "1", "--no-progress"))
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
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--no-progress")), "add")
	corruptObject(t, cacheRoot, digest.Bytes(content))

	// path used to return this object, and every script downstream would have
	// read the wrong bytes without ever being told.
	assertError(t, runJSON(t, appendArgs(base, "path", "app/geo@1")), "cache_object_corrupt")
	assertError(t, runJSON(t, appendArgs(base, "export", filepath.Join(directory, "bundle.tar"))), "cache_object_corrupt")
	assertError(t, runJSON(t, appendArgs(base, "cache", "scrub")), "cache_object_corrupt")

	info := runJSON(t, appendArgs(base, "info"))
	assertSuccess(t, info, "info")
	assets := info.value["data"].(map[string]any)["assets"].([]any)
	if assets[0].(map[string]any)["cacheStatus"] != "corrupt" {
		t.Fatalf("info did not report the damage: %#v", assets[0])
	}

	// pull replaces the object, and the whole project comes back clean.
	assertSuccess(t, runJSON(t, appendArgs(base, "pull", "--no-progress")), "pull")
	assertSuccess(t, runJSON(t, appendArgs(base, "path", "app/geo@1")), "path")
	verified := runJSON(t, appendArgs(base, "cache", "scrub"))
	assertSuccess(t, verified, "cache.scrub")
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
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--no-progress")), "add")
	corruptObject(t, cacheRoot, digest.Bytes(content))

	repaired := runJSON(t, appendArgs(base, "cache", "scrub", "--repair"))
	assertSuccess(t, repaired, "cache.scrub")
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

	result := runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--no-progress"))
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
	result := run(t, appendArgs(base, "add", "app/geo@1", server.URL, "--no-progress"))
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
func TestLockWritesAMissingLockFile(t *testing.T) {
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

	missing := run(t, appendArgs(base, "pull", "--no-progress"))
	if missing.status != ExitFailure || !strings.Contains(missing.stderr, "Run dac lock.") {
		t.Fatalf("a missing lock file did not name the command that writes it: %#v", missing)
	}
	human := run(t, appendArgs(base, "lock", "--no-progress"))
	if human.status != ExitOK || human.stdout != "Locked app/geo@1.\n" {
		t.Fatalf("unexpected human lock: %#v", human)
	}
	projecttest.Check(t, manifestPath, lockPath)
	// Locking resolves, and resolving installs, so the pull behind it has
	// nothing left to fetch.
	pulled := run(t, appendArgs(base, "pull", "--no-progress"))
	if pulled.status != ExitOK || pulled.stdout != "Pulled 1 asset.\n" {
		t.Fatalf("unexpected human pull: %#v", pulled)
	}
	// A second lock over an unchanged project resolves nothing and leaves the
	// file alone, which is the run a CI check makes.
	again := run(t, appendArgs(base, "lock", "--no-progress"))
	if again.status != ExitOK || again.stdout != "The lock file already describes 1 asset.\n" {
		t.Fatalf("unexpected repeated lock: %#v", again)
	}
	// A refresh reaches the origin for every asset whatever it finds there, so it
	// reports what it resolved and that the file did not move. Announcing a lock
	// would describe a diff nobody is going to find.
	refreshed := run(t, appendArgs(base, "lock", "--refresh", "--no-progress"))
	if refreshed.status != ExitOK || refreshed.stdout != "Resolved app/geo@1. The lock file is unchanged.\n" {
		t.Fatalf("unexpected refresh: %#v", refreshed)
	}
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
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--no-progress")), "add")
	assertSuccess(t, runJSON(t, appendArgs(base, "verify", "--refresh", "--no-progress")), "verify")

	body.Store([]byte("moved bytes"))
	before := projecttest.MustRead(t, lockPath)
	result := runJSON(t, appendArgs(base, "verify", "--refresh", "--no-progress"))
	assertError(t, result, "lock_drift")
	if !bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("--refresh rewrote the lock file")
	}
	// The command that writes refuses the same drift, because a locked version
	// names one set of bytes. Accepting the change is a decision, and --rebind is
	// where an operator makes it.
	assertError(t, runJSON(t, appendArgs(base, "lock", "--refresh", "--no-progress")), "version_rebind")
	if !bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("a refused rebind rewrote the lock file")
	}
	rebound := runJSON(t, appendArgs(base, "lock", "--refresh", "--rebind", "--no-progress"))
	assertSuccess(t, rebound, "lock")
	if rebound.value["data"].(map[string]any)["changed"] != true {
		t.Fatalf("the rebind did not report a changed lock file: %#v", rebound.value)
	}
	if bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("--rebind left the lock file as it found it")
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
	project := []string{
		"--manifest", filepath.Join(directory, "dac.json"),
		"--lock", filepath.Join(directory, "dac-lock.json"),
		"--cache-dir", filepath.Join(directory, "cache"),
	}
	bounded := append(configFlags(t, "[transfer]\nmax-size = \"1KiB\"\n"), project...)
	unbounded := append(configFlags(t, "[transfer]\nmax-size = \"none\"\n"), project...)

	assertSuccess(t, runJSON(t, appendArgs(project, "init")), "init")
	assertError(t, runJSON(t, appendArgs(bounded, "add", "app/geo@1", server.URL, "--no-progress")), "asset_too_large")
	assertSuccess(t, runJSON(t, appendArgs(unbounded, "add", "app/geo@1", server.URL, "--no-progress")), "add")
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
		server.URL, "--pin", "--no-progress")), "add")
	assertSuccess(t, runJSON(t, appendArgs(base, "verify", "--refresh", "--no-progress")), "verify")

	body.Store([]byte("moved bytes"))
	before := projecttest.MustRead(t, lockPath)
	result := runJSON(t, appendArgs(base, "verify", "--refresh", "--no-progress"))
	assertError(t, result, "lock_drift")
	if !bytes.Contains([]byte(result.stdout), []byte(`"app/geo@1"`)) {
		t.Fatalf("lock_drift did not name the drifted asset: %s", result.stdout)
	}
	if !bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("--refresh rewrote the lock file")
	}
	// The same origin against the same pin, from the command that writes: a pin
	// is a rule there, and bytes that fail it must not reach the lock file.
	assertError(t, runJSON(t, appendArgs(base, "lock", "--refresh", "--no-progress")), "content_mismatch")
	if !bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("a failed lock --refresh rewrote the lock file")
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
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--pin", "--no-progress")), "add")

	manifest, _ := projecttest.Check(t, manifestPath, lockPath)
	if manifest.Assets[coord.MustParse("app/geo@1")].Integrity != digest.Bytes(content) {
		t.Fatalf("add --pin did not write the integrity value: %#v", manifest.Assets[coord.MustParse("app/geo@1")])
	}
}

// A project is its two files together. Pointing --manifest at another directory
// used to leave the lock behind in the working directory, which produced a pair
// that did not describe each other and no message saying so. The rewrite config
// already resolved beside the manifest; the lock file now agrees.
func TestLockFileFollowsTheManifest(t *testing.T) {
	directory := t.TempDir()
	nested := filepath.Join(directory, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(nested, "dac.json")
	base := []string{"--manifest", manifestPath, "--cache-dir", filepath.Join(directory, "cache")}

	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	projecttest.Check(t, manifestPath, filepath.Join(nested, "dac-lock.json"))
	if _, err := os.Stat(filepath.Join(directory, "dac-lock.json")); !os.IsNotExist(err) {
		t.Fatal("the lock file was written beside the working directory rather than the manifest")
	}
	// Every later command finds the same pair without being told where it is.
	assertSuccess(t, runJSON(t, appendArgs(base, "verify")), "verify")
}

// Naming the lock file explicitly still puts it exactly where it was asked for,
// which is what lets a caller keep the two apart on purpose.
func TestExplicitLockFlagWins(t *testing.T) {
	directory := t.TempDir()
	nested := filepath.Join(directory, "sub")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(nested, "dac.json")
	lockPath := filepath.Join(directory, "elsewhere.json")
	base := []string{"--manifest", manifestPath, "--lock", lockPath, "--cache-dir", filepath.Join(directory, "cache")}

	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	projecttest.Check(t, manifestPath, lockPath)
}

// The cache had no management surface: collection removed objects by an age
// nobody could inspect, emptying it meant a collection with an age short enough
// that everything fell outside it, and dropping one asset was not possible at
// all.
func TestCacheListReportsWhatTheCacheHolds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write(bytes.Repeat([]byte(request.URL.Path), 100))
	}))
	defer server.Close()

	paths := newProject(t)
	base := paths.base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/one@1", server.URL+"/a", "--no-progress")), "add")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/two@1", server.URL+"/b", "--no-progress")), "add")

	result := runJSON(t, appendArgs(base, "cache", "list"))
	assertSuccess(t, result, "cache.list")
	data := result.value["data"].(map[string]any)
	if data["objectCount"] != float64(2) || data["missingCount"] != float64(0) {
		t.Fatalf("unexpected listing: %#v", data)
	}
	objects := data["objects"].([]any)
	owned := map[string]bool{}
	for _, entry := range objects {
		object := entry.(map[string]any)
		if object["size"].(float64) <= 0 || object["digest"] == "" {
			t.Fatalf("an object is missing its size or digest: %#v", object)
		}
		for _, name := range object["coordinates"].([]any) {
			owned[name.(string)] = true
		}
	}
	if !owned["app/one@1"] || !owned["app/two@1"] {
		t.Fatalf("the listing did not name the assets: %#v", objects)
	}

	// A listing must not count as using the cache. Reaching an object refreshes
	// the liveness timestamp collection runs on, so a listing built that way
	// would quietly keep everything alive.
	before := lastUsedTimes(t, paths)
	assertSuccess(t, runJSON(t, appendArgs(base, "cache", "list")), "cache.list")
	for value, when := range lastUsedTimes(t, paths) {
		if !when.Equal(before[value]) {
			t.Fatalf("listing refreshed the liveness timestamp for %s", value)
		}
	}
}

// lastUsedTimes reads each cached object's sidecar timestamp.
func lastUsedTimes(t *testing.T, paths projectFlags) map[string]time.Time {
	t.Helper()
	store := cache.New(paths.cacheRoot)
	digests, err := store.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	times := map[string]time.Time{}
	for _, value := range digests {
		described, found, err := store.Describe(value)
		if err != nil {
			t.Fatal(err)
		}
		if found {
			times[value] = described.LastUsed
		}
	}
	return times
}

func TestCacheClearEmptiesTheCache(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("asset bytes"))
	}))
	defer server.Close()

	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/one@1", server.URL, "--no-progress")), "add")

	// A dry run reports the same objects and removes none of them.
	dry := runJSON(t, appendArgs(base, "cache", "clear", "--dry-run"))
	assertSuccess(t, dry, "cache.clear")
	if dry.value["data"].(map[string]any)["objectCount"] != float64(1) {
		t.Fatalf("the dry run found nothing: %#v", dry.value)
	}
	if listed := runJSON(t, appendArgs(base, "cache", "list")); listed.value["data"].(map[string]any)["objectCount"] != float64(1) {
		t.Fatal("the dry run removed the object")
	}

	assertSuccess(t, runJSON(t, appendArgs(base, "cache", "clear")), "cache.clear")
	empty := runJSON(t, appendArgs(base, "cache", "list"))
	assertSuccess(t, empty, "cache.list")
	data := empty.value["data"].(map[string]any)
	if data["objectCount"] != float64(0) || data["missingCount"] != float64(1) {
		t.Fatalf("the cache was not cleared: %#v", data)
	}
	// A cleared cache costs a pull and nothing more.
	assertSuccess(t, runJSON(t, appendArgs(base, "pull", "--no-progress")), "pull")
}

// Two coordinates that resolved to the same bytes share one object, so removing
// one can uncache the other. The cache is the one place DAC deletes data, so
// that needs saying out loud rather than discovering afterwards.
func TestCacheRemoveRefusesToUncacheAnUnnamedAsset(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("shared" + request.URL.Path))
	}))
	defer server.Close()

	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/one@1", server.URL+"/a", "--no-progress")), "add")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/alone@1", server.URL+"/b", "--no-progress")), "add")
	// Two coordinates, one source, so one object between them.
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/two@1", server.URL+"/a", "--no-progress")), "add")

	refused := runJSON(t, appendArgs(base, "cache", "remove", "app/one@1"))
	assertError(t, refused, "cache_object_shared")
	shared := refused.value["error"].(map[string]any)["details"].(map[string]any)["shared"].([]any)
	if len(shared) != 1 || shared[0] != "app/two@1" {
		t.Fatalf("the refusal did not name the sharer: %#v", shared)
	}
	if listed := runJSON(t, appendArgs(base, "cache", "list")); listed.value["data"].(map[string]any)["objectCount"] != float64(2) {
		t.Fatal("the refused removal took an object anyway")
	}

	forced := runJSON(t, appendArgs(base, "cache", "remove", "app/one@1", "--force"))
	assertSuccess(t, forced, "cache.remove")
	if names := forced.value["data"].(map[string]any)["shared"].([]any); len(names) != 1 || names[0] != "app/two@1" {
		t.Fatalf("the removal did not report what it cost: %#v", forced.value)
	}

	// Naming both is not sharing, so it needs no --force.
	assertSuccess(t, runJSON(t, appendArgs(base, "pull", "--no-progress")), "pull")
	both := runJSON(t, appendArgs(base, "cache", "remove", "app/one@1", "app/two@1"))
	assertSuccess(t, both, "cache.remove")
	if count := both.value["data"].(map[string]any)["objectCount"]; count != float64(1) {
		t.Fatalf("removing both coordinates removed %v objects, want 1", count)
	}
	// The asset that shared nothing keeps its bytes.
	if listed := runJSON(t, appendArgs(base, "cache", "list")); listed.value["data"].(map[string]any)["objectCount"] != float64(1) {
		t.Fatal("an unrelated asset lost its object")
	}
}

func TestCacheRemoveRejectsAssetsTheProjectDoesNotHave(t *testing.T) {
	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertError(t, runJSON(t, appendArgs(base, "cache", "remove", "app/absent@1")), "asset_not_found")
	if result := runJSON(t, appendArgs(base, "cache", "remove")); result.status != ExitUsage {
		t.Fatalf("a removal with no coordinate exited %d", result.status)
	}
}

// Config merged across the XDG search path raises two questions a person cannot
// answer by looking: which files was it, and what did they settle on.
func TestConfigReportsItsFilesAndEffectiveValues(t *testing.T) {
	settings := configFlags(t, "[transfer]\nretries = 5\nconcurrency = 2\n")
	base := append(settings, newProject(t).base...)

	paths := runJSON(t, appendArgs(base, "config", "path"))
	assertSuccess(t, paths, "config.path")
	files := paths.value["data"].(map[string]any)["files"].([]any)
	if len(files) != 1 || files[0] != settings[1] {
		t.Fatalf("config path did not name the file: %#v", files)
	}

	shown := runJSON(t, appendArgs(base, "config", "show"))
	assertSuccess(t, shown, "config.show")
	sources := map[string]string{}
	values := map[string]string{}
	for _, entry := range shown.value["data"].(map[string]any)["settings"].([]any) {
		setting := entry.(map[string]any)
		values[setting["key"].(string)] = setting["value"].(string)
		sources[setting["key"].(string)] = setting["source"].(string)
	}
	if values["transfer.retries"] != "5" || sources["transfer.retries"] != settings[1] {
		t.Fatalf("retries reported as %q from %q", values["transfer.retries"], sources["transfer.retries"])
	}
	if values["transfer.timeout"] != "5m0s" || sources["transfer.timeout"] != "default" {
		t.Fatalf("timeout reported as %q from %q", values["transfer.timeout"], sources["transfer.timeout"])
	}
}

// A flag beats the config file, which is the whole point of leaving one on the
// command line.
func TestConcurrencyFlagBeatsTheConfigFile(t *testing.T) {
	var peak, current atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		running := current.Add(1)
		for {
			seen := peak.Load()
			if running <= seen || peak.CompareAndSwap(seen, running) {
				break
			}
		}
		<-release
		current.Add(-1)
		_, _ = writer.Write([]byte("asset bytes"))
	}))
	defer server.Close()

	base := append(configFlags(t, "[transfer]\nconcurrency = 1\n"), newProject(t).base...)
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	close(release)
	for _, name := range []string{"app/one@1", "app/two@1", "app/three@1"} {
		assertSuccess(t, runJSON(t, appendArgs(base, "add", name, server.URL+"/"+name, "--no-progress")), "add")
	}
	// The config says one asset at a time; the flag says three. Both spellings
	// have to reach the same place, and the flag has to win.
	assertSuccess(t, runJSON(t, appendArgs(base, "cache", "clear")), "cache.clear")
	assertSuccess(t, runJSON(t, appendArgs(base, "pull", "--concurrency", "3", "--no-progress")), "pull")
}

// The credentials table is a list of programs DAC runs, so a config file
// anybody can write is a way to choose them.
func TestWritableConfigIsRefused(t *testing.T) {
	settings := configFlags(t, "[transfer]\nretries = 1\n")
	if err := os.Chmod(settings[1], 0o666); err != nil {
		t.Fatal(err)
	}
	base := append(settings, newProject(t).base...)
	result := runJSON(t, appendArgs(base, "pull"))
	assertError(t, result, "config_invalid")
	if cause, _ := result.value["error"].(map[string]any)["cause"].(string); !strings.Contains(cause, "writable") {
		t.Fatalf("the cause did not explain the refusal: %v", result.value)
	}
}
