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
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/cache"
	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/output"
	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/dac/internal/projecttest"
	"github.com/tomdoesdev/dac/internal/trust"
)

type invocation struct {
	status int
	stdout string
	stderr string
	value  map[string]any
}

// TestMain points the config search path and the trusted-hosts file at an empty directory.
// DAC reads a config file from the XDG base directories, so without this every test in this package would read whatever the machine running it has installed, and a developer with a config would get different results from CI.
// The trusted-hosts file is pointed the same way and seeded with the loopback names, because every test in this package downloads from an httptest server and DAC refuses a host nothing trusts.
func TestMain(main *testing.M) {
	root, err := os.MkdirTemp("", "dac-cli-test")
	if err != nil {
		panic(err)
	}
	for name, value := range map[string]string{
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"XDG_CONFIG_DIRS": filepath.Join(root, "system"),
		"XDG_DATA_HOME":   filepath.Join(root, "data"),
		"HOME":            root,
		"DAC_TRUST_FILE":  filepath.Join(root, "trusted-hosts.json"),
	} {
		if err := os.Setenv(name, value); err != nil {
			panic(err)
		}
	}
	if err := os.Unsetenv("DAC_CONFIG"); err != nil {
		panic(err)
	}
	if _, err := trust.New(os.Getenv("DAC_TRUST_FILE")).Update(context.Background(),
		func(list trust.List) (trust.List, error) {
			updated, _ := list.Add([]string{"127.0.0.1", "localhost", "::1"}, time.Now().UTC())
			return updated, nil
		}); err != nil {
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
		// The server URL carries no path, so the header is the only thing that names this asset.
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
	// Add changes only source intent; pull performs the first resolution and lock write.
	settleCLIProject(t, base)
	manifest, lock := projecttest.Check(t, manifestPath, lockPath)
	if _, exists := manifest.Assets[coord.MustParse("app/geo@2026.08")]; !exists ||
		lock.Assets[coord.MustParse("app/geo@2026.08")].Digest == "" {
		t.Fatalf("add and pull did not settle both files: %#v %#v", manifest, lock)
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
		infoAsset["name"] != "geo" || infoAsset["version"] != "2026.08" || infoAsset["sourceUrl"] != server.URL || infoAsset["cacheStatus"] != "cached" ||
		infoAsset["filename"] != "geo.bin" ||
		infoAsset["digest"] != lock.Assets[coord.MustParse("app/geo@2026.08")].Digest || infoAsset["size"] != float64(len(content)) || infoAsset["path"] == "" {
		t.Fatalf("unexpected info asset: %#v", infoAsset)
	}
	result = runJSON(t, appendArgs(base, "verify"))
	assertSuccess(t, result, "verify")
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
		"trust: trusted\n" +
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

	lockBeforeRemove := projecttest.MustRead(t, lockPath)
	result = runJSON(t, appendArgs(base, "remove", "app/geo@2026.08"))
	assertSuccess(t, result, "remove")
	manifest, err = project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Assets) != 0 {
		t.Fatalf("remove left assets: %#v", manifest.Assets)
	}
	if !bytes.Equal(lockBeforeRemove, projecttest.MustRead(t, lockPath)) {
		t.Fatal("remove changed the lock file")
	}
	// One request for the initial pull and one for each pull that had to replace the object this test deleted.
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
		// The source is an argument rather than a flag, so an add that names no source is an add with the wrong number of arguments.
		appendArgs(base, "add", "app/asset@1"),
		appendArgs(base, "add"),
		appendArgs(base, "add", "app/asset@1", "https://example.com/asset", "https://example.com/other"),
		appendArgs(base, "add", "app/asset@1", "  "),
		// A version is not optional where a command writes: an add that picked one for itself would rewrite whichever version it guessed.
		appendArgs(base, "add", "app/asset", "https://example.com/asset"),
		appendArgs(base, "add", "app/asset@1", "https://example.com/asset", "--rewrite-config", filepath.Join(directory, "rewrite")),
		appendArgs(base, "info", "asset"),
		appendArgs(base, "info", "app/asset@1", "app/other@1"),
		appendArgs(base, "list"),
		appendArgs(base, "rewrite"),
		// Every lock operation lives on pull now, so the command that used to spell them is gone.
		appendArgs(base, "lock"),
		appendArgs(base, "lock", "--refresh"),
		// The transfer tuning options moved into the config file, so the command line no longer answers to them at all.
		appendArgs(base, "pull", "--max-size", "1GiB"),
		appendArgs(base, "pull", "--timeout", "1m"),
		appendArgs(base, "pull", "--retries", "1"),
		appendArgs(base, "pull", "--download-parts", "2"),
		appendArgs(base, "pull", "--credential-helper", "helper"),
		appendArgs(base, "pull", "--no-rewrite"),
		appendArgs(base, "pull", "--distdir", directory),
		appendArgs(base, "pull", "--progress=false"),
		// Refreshing the lock file has one spelling, and it is not either of the ones an older DAC used.
		appendArgs(base, "pull", "--update-lock"),
		appendArgs(base, "pull", "--refresh-lock"),
		appendArgs(base, "add", "app/asset@1", "https://example.com/asset", "--rebind"),
		appendArgs(base, "pull", "--rebind"),
		appendArgs(base, "pack"),
		appendArgs(base, "cache", "import", "delivery"),
		appendArgs(base, "unpack", "--tree"),
		appendArgs(base, "unpack", "delivery.dacpack"),
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

func TestAddDefaultsToManifestOnly(t *testing.T) {
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

	result := runJSON(t, appendArgs(base, "add", "app/asset@1", server.URL))
	assertSuccess(t, result, "add")
	manifest, err := project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := manifest.Assets[coord.MustParse("app/asset@1")]; !exists {
		t.Fatalf("add did not update the manifest: %#v", manifest)
	}
	if !bytes.Equal(lockBefore, projecttest.MustRead(t, lockPath)) {
		t.Fatal("add changed the lock file")
	}
	if requests.Load() != 0 {
		t.Fatalf("add made %d requests", requests.Load())
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
	assertSuccess(t, runJSON(t, appendArgs(base, "pull", "--no-progress")), "pull")
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

func TestPullHelpOmitsRemovedRequestFlags(t *testing.T) {
	result := run(t, []string{"pull", "--help"})
	if result.status != ExitOK {
		t.Fatalf("help failed: %#v", result)
	}
	for _, flag := range []string{"--no-rewrite", "--credential-helper"} {
		if strings.Contains(result.stderr, flag) {
			t.Fatalf("pull help contains removed flag %s", flag)
		}
	}
}

// runJSON selects JSON mode and decodes the result.
// TestGroupCommandShowsItsOwnHelp covers what a bare group command answers.
// The commands that only group others share one action with the root, and the
// question somebody typing "dac trust" is asking is what trust can do, not what
// dac can do.
func TestGroupCommandShowsItsOwnHelp(t *testing.T) {
	for _, group := range []struct{ name, command string }{
		{name: "trust", command: "list"},
		{name: "cache", command: "scrub"},
		{name: "config", command: "show"},
	} {
		t.Run(group.name, func(t *testing.T) {
			result := run(t, []string{group.name})
			if result.status != ExitOK {
				t.Fatalf("status = %d, want %d", result.status, ExitOK)
			}
			if !strings.Contains(result.stderr, "dac "+group.name+" -") {
				t.Fatalf("help does not name the group itself:\n%s", result.stderr)
			}
			if !strings.Contains(result.stderr, group.command) {
				t.Fatalf("help does not list %q, which is one of its commands:\n%s", group.command, result.stderr)
			}
		})
	}
}

// TestIncompleteCommandInJSONModeWritesADocument covers the contract rather than
// the wording. Help is not something a JSON consumer can read, so an invocation
// that would answer with help has to answer with the usage error it is instead:
// exiting successfully with nothing on standard output leaves a caller with no
// result and no failure to handle.
func TestIncompleteCommandInJSONModeWritesADocument(t *testing.T) {
	for _, name := range []string{"", "trust", "cache", "config"} {
		t.Run("dac "+name, func(t *testing.T) {
			args := []string{"--json"}
			if name != "" {
				args = append(args, name)
			}
			result := runJSONArgs(t, args)
			if result.status != ExitUsage {
				t.Fatalf("status = %d, want %d", result.status, ExitUsage)
			}
			if result.value["command"] != name {
				t.Fatalf("command = %v, want %q", result.value["command"], name)
			}
			assertError(t, result, "invalid_arguments")
		})
	}
}

// TestUnknownSubcommandNamesTheGroupItWasGiven pins the command a failure is
// reported against: the group that was asked, rather than the word that is not
// one of its commands.
func TestUnknownSubcommandNamesTheGroupItWasGiven(t *testing.T) {
	result := runJSON(t, []string{"trust", "bogus"})
	if result.status != ExitUsage {
		t.Fatalf("status = %d, want %d", result.status, ExitUsage)
	}
	if result.value["command"] != "trust" {
		t.Fatalf("command = %v, want trust", result.value["command"])
	}
	assertError(t, result, "invalid_arguments")
}

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

// projectFlags sets up a manifest, lock, and cache under one directory and returns the global flags that point DAC at them.
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

// settleCLIProject runs the sole command allowed to reconcile manifest changes into the lock.
func settleCLIProject(t *testing.T, base []string) {
	t.Helper()
	assertSuccess(t, runJSON(t, appendArgs(base, "pull", "--no-progress")), "pull")
}

// TestPathAcceptsAnAssetWithoutItsVersion covers the shorter form and the one case it must refuse.
func TestPathAcceptsAnAssetWithoutItsVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, "bytes for "+request.URL.Path)
	}))
	defer server.Close()

	paths := newProject(t)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "java/sdk@11",
		server.URL+"/jdk11.tar.gz", "--no-progress")), "add")
	settleCLIProject(t, paths.base)

	objectPath := objectPathFor(t, paths, coord.MustParse("java/sdk@11"))
	bare := run(t, appendArgs(paths.base, "path", "java/sdk"))
	if bare.status != ExitOK || bare.stdout != objectPath+"\n" {
		t.Fatalf("path without a version returned %#v", bare)
	}
	versioned := run(t, appendArgs(paths.base, "path", "java/sdk@11"))
	if versioned.stdout != bare.stdout {
		t.Fatalf("the two spellings disagree: %q and %q", versioned.stdout, bare.stdout)
	}
	// The result names the version it resolved to, so a caller that left it off can still tell which asset answered.
	result := runJSON(t, appendArgs(paths.base, "path", "java/sdk"))
	assertSuccess(t, result, "path")
	if data := result.value["data"].(map[string]any); data["coordinate"] != "java/sdk@11" || data["version"] != "11" {
		t.Fatalf("path did not report the version it resolved: %#v", data)
	}

	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "java/sdk@17",
		server.URL+"/jdk17.tar.gz", "--no-progress")), "add")
	settleCLIProject(t, paths.base)

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
	// An asset the project does not have is a different answer from one it has too many of, and it keeps the code it always had.
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
	settleCLIProject(t, base)

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

// TestColourAddsNothingButColour is the property that makes colour safe to add at all: a result read on a terminal and the same result read anywhere else say the same thing, word for word.
func TestColourAddsNothingButColour(t *testing.T) {
	content := []byte("colourful asset bytes")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	newAssetProject := func() []string {
		base := newProject(t).base
		assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
		assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--no-progress")), "add")
		settleCLIProject(t, base)
		return base
	}
	// The two runs take a project from the same source, which for a command that only reads is the same project twice: a cache listing reports a last use, and two caches filled a moment apart do not agree about it.
	compare := func(base func() []string, command ...string) {
		t.Helper()
		coloured := run(t, appendArgs(base(), append([]string{"--color=always"}, command...)...))
		if !strings.Contains(coloured.stdout, "\x1b[") {
			t.Fatalf("dac %v wrote no colour: %q", command, coloured.stdout)
		}
		bare := run(t, appendArgs(base(), append([]string{"--color=never"}, command...)...))
		if strings.Contains(bare.stdout, "\x1b") {
			t.Fatalf("dac %v coloured a refusal: %q", command, bare.stdout)
		}
		if stripped := escapes.ReplaceAllString(coloured.stdout, ""); stripped != bare.stdout {
			t.Fatalf("dac %v reads differently in colour:\n colour: %q\n  plain: %q", command, stripped, bare.stdout)
		}
	}

	shared := newAssetProject()
	reused := func() []string { return shared }
	compare(reused, "info")
	compare(reused, "verify")
	compare(reused, "cache", "list")
	compare(reused, "cache", "scrub")
	compare(reused, "pull", "--no-progress")
	// A removal is not idempotent, so it gets a project per run.
	compare(newAssetProject, "remove", "app/geo@1")
}

// The default is auto, and the writers a test hands DAC are buffers rather than terminals -- which is the same thing every pipe, file, and CI job is.
func TestColourStaysOffWhereNothingWillRenderIt(t *testing.T) {
	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	if human := run(t, appendArgs(base, "verify")); strings.Contains(human.stdout, "\x1b") {
		t.Fatalf("auto coloured a buffer: %q", human.stdout)
	}
	// Forcing it is the whole reason the option exists: a pager or a CI log viewer renders sequences that the pipe in front of it cannot admit to.
	if human := run(t, appendArgs(base, "--colour=always", "verify")); !strings.Contains(human.stdout, "\x1b[") {
		t.Fatalf("--colour=always wrote no colour: %q", human.stdout)
	}
}

// JSON is parsed.
func TestJSONIgnoresForcedColour(t *testing.T) {
	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	result := runJSONArgs(t, append([]string{"--json", "--color=always"}, appendArgs(base, "info")...))
	assertSuccess(t, result, "info")
	if strings.Contains(result.stdout, "\x1b") || strings.Contains(result.stderr, "\x1b") {
		t.Fatalf("JSON mode wrote escapes: stdout=%q stderr=%q", result.stdout, result.stderr)
	}
}

func TestColourRejectsAValueItCannotRead(t *testing.T) {
	base := newProject(t).base
	result := runJSON(t, appendArgs(base, "--color", "alwyas", "info"))
	assertError(t, result, "invalid_arguments")
	if result.status != ExitUsage {
		t.Fatalf("unexpected status %d: %q", result.status, result.stderr)
	}
}

// escapes matches the SGR sequences a palette writes.
var escapes = regexp.MustCompile("\x1b\\[[0-9;]*m")

func TestCacheGCRejectsAnInvalidAge(t *testing.T) {
	base := newProject(t).base
	if result := run(t, appendArgs(base, "cache", "gc", "--max-age", "forever")); result.status != ExitUsage {
		t.Fatalf("unexpected status %d: %q", result.status, result.stderr)
	}
}

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

func TestRemovedRequestSettingsAreInvalid(t *testing.T) {
	for _, settings := range []string{
		"[credentials]\ndefault = \"helper\"\n",
		"[[rewrite]]\npattern = \"x\"\nreplacement = \"y\"\n",
		"[hosts]\nblock = [\"*\"]\n",
	} {
		base := append(configFlags(t, settings), newProject(t).base...)
		assertError(t, runJSON(t, appendArgs(base, "pull")), "config_invalid")
	}
}

func TestAddPinReportsTheResolvedDigest(t *testing.T) {
	content := []byte("asset bytes")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	human := run(t, appendArgs(base, "add", "app/geo@1", server.URL, "--pin", "--no-progress"))
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
	settleCLIProject(t, paths.base)

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
	// The failure has to name the asset, or a multi-asset pull reports a digest pair without saying which asset it belongs to.
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

// corruptObject rewrites a cache object in place, keeping its size, which is the damage that used to pass every check DAC made.
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

// TestCorruptCacheObjectIsCaughtEndToEnd walks the failure the store used to hand back without comment: a cache object edited in place, keeping its size.
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
	settleCLIProject(t, base)
	corruptObject(t, cacheRoot, digest.Bytes(content))

	// path used to return this object, and every script downstream would have read the wrong bytes without ever being told.
	assertError(t, runJSON(t, appendArgs(base, "path", "app/geo@1")), "cache_object_corrupt")
	assertError(t, runJSON(t, appendArgs(base, "unpack", "--dest", filepath.Join(directory, "unpacked"))), "cache_object_corrupt")
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
	settleCLIProject(t, base)
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

// TestJSONErrorsCarryTheirCause covers the failure an operator actually meets in CI: a pull that cannot reach its origin.
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

	result := runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--pin", "--no-progress"))
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
	result := run(t, appendArgs(base, "add", "app/geo@1", server.URL, "--pin", "--no-progress"))
	if result.status != ExitFailure || !strings.Contains(result.stderr, "404") {
		t.Fatalf("the human error hid its cause: status=%d stderr=%q", result.status, result.stderr)
	}
	// The content-check message used to print its detail twice, once inside the message and once again as the cause.
	if strings.Count(result.stderr, "HTTP 404") != 1 {
		t.Fatalf("the cause was printed more than once: %q", result.stderr)
	}
}

// The path a project takes when its lock file is not committed, or when someone edits the manifest by hand: a plain pull says what to run, and that command writes the file and names what it locked.
// TestPullWritesAMissingLockFile walks the whole of what a merged pull owns: it settles a
// checkout that has no lock file yet, leaves that file alone on every run after it, and
// rewrites it only when asked to refresh.
func TestPullWritesAMissingLockFile(t *testing.T) {
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

	// One command settles the checkout: it resolves the manifest, writes the lock file it found none of, and installs what it resolved.
	human := run(t, appendArgs(base, "pull", "--no-progress"))
	if human.status != ExitOK || human.stdout != "Locked app/geo@1. Pulled 1 asset.\n" {
		t.Fatalf("unexpected first pull: %#v", human)
	}
	projecttest.Check(t, manifestPath, lockPath)

	// Every run after that reads the lock file and writes nothing, which is the run a job makes.
	before := projecttest.MustRead(t, lockPath)
	again := run(t, appendArgs(base, "pull", "--no-progress"))
	if again.status != ExitOK || again.stdout != "Pulled 1 asset.\n" {
		t.Fatalf("unexpected repeated pull: %#v", again)
	}
	if !bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("a plain pull rewrote the lock file")
	}

	// A refresh reaches the origin whatever it finds there, so it reports what it resolved and that the file did not move.
	refreshed := run(t, appendArgs(base, "pull", "--refresh", "--no-progress"))
	if refreshed.status != ExitOK || refreshed.stdout != "Resolved app/geo@1. The lock file is unchanged. Pulled 1 asset.\n" {
		t.Fatalf("unexpected refresh: %#v", refreshed)
	}
}

// A plain pull reconciles a stale lock because pull owns all lock-file changes.
func TestPullSettlesAStaleLock(t *testing.T) {
	content := []byte("asset bytes")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(content)
	}))
	defer server.Close()

	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	base := []string{"--manifest", manifestPath, "--lock", lockPath, "--cache-dir", filepath.Join(directory, "cache")}
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	// Add writes the manifest alone, leaving pull to reconcile the two files.
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--offline")), "add")

	before := projecttest.MustRead(t, lockPath)
	settled := run(t, appendArgs(base, "pull", "--no-progress"))
	if settled.status != ExitOK || settled.stdout != "Locked app/geo@1. Pulled 1 asset.\n" {
		t.Fatalf("a plain pull did not settle the stale lock: %#v", settled)
	}
	if bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("pull left the stale lock unchanged")
	}
	projecttest.Check(t, manifestPath, lockPath)
}

// Offline mode resolves nothing, so it has nothing to write a lock file from.
func TestOfflinePullCannotSettleAProjectWithNoLockFile(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	base := []string{
		"--manifest", manifestPath,
		"--lock", filepath.Join(directory, "dac-lock.json"),
		"--cache-dir", filepath.Join(directory, "cache"),
	}
	manifest := project.Manifest{
		SchemaVersion: project.ManifestVersion,
		Assets:        map[coord.Coordinate]project.Asset{coord.MustParse("app/geo@1"): {URL: "https://example.com/geo"}},
	}
	data, err := project.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	assertError(t, runJSON(t, appendArgs(base, "pull", "--offline", "--no-progress")), "lock_missing")
	// The two options ask for opposite things, and the run says so instead of quietly dropping one.
	assertError(t, runJSON(t, appendArgs(base, "pull", "--offline", "--refresh", "--no-progress")), "invalid_arguments")
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
	settleCLIProject(t, base)
	assertSuccess(t, runJSON(t, appendArgs(base, "verify", "--refresh", "--no-progress")), "verify")

	body.Store([]byte("moved bytes"))
	before := projecttest.MustRead(t, lockPath)
	result := runJSON(t, appendArgs(base, "verify", "--refresh", "--no-progress"))
	assertError(t, result, "lock_drift")
	if !bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("--refresh rewrote the lock file")
	}
	refreshed := runJSON(t, appendArgs(base, "pull", "--refresh", "--no-progress"))
	assertSuccess(t, refreshed, "pull")
	if refreshed.value["data"].(map[string]any)["changed"] != true {
		t.Fatalf("the refresh did not report a changed lock file: %#v", refreshed.value)
	}
	if bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("pull --refresh left the lock file as it found it")
	}
}

// TestMaxSizeBoundsADownloadAndOnlyNoneRemovesTheBound checks the guard from both ends.
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
	assertError(t, runJSON(t, appendArgs(bounded, "add", "app/geo@1", server.URL, "--pin", "--no-progress")), "asset_too_large")
	assertSuccess(t, runJSON(t, appendArgs(unbounded, "add", "app/geo@1", server.URL, "--pin", "--no-progress")), "add")
}

// TestVerifyRefreshReportsDriftForAPinnedAsset covers the asset the drift check was least able to report on.
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
	settleCLIProject(t, base)
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
	// The same origin against the same pin, from the command that writes: a pin is a rule there, and bytes that fail it must not reach the lock file.
	assertError(t, runJSON(t, appendArgs(base, "pull", "--refresh", "--no-progress")), "content_mismatch")
	if !bytes.Equal(before, projecttest.MustRead(t, lockPath)) {
		t.Fatal("a failed pull --refresh rewrote the lock file")
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
	lockBefore := projecttest.MustRead(t, lockPath)
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--pin", "--no-progress")), "add")

	manifest, err := project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Assets[coord.MustParse("app/geo@1")].Integrity != digest.Bytes(content) {
		t.Fatalf("add --pin did not write the integrity value: %#v", manifest.Assets[coord.MustParse("app/geo@1")])
	}
	if !bytes.Equal(lockBefore, projecttest.MustRead(t, lockPath)) {
		t.Fatal("add --pin changed the lock file")
	}
}

// A project is its two files together.
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

// Naming the lock file explicitly still puts it exactly where it was asked for, which is what lets a caller keep the two apart on purpose.
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

// The cache had no management surface: collection removed objects by an age nobody could inspect, emptying it meant a collection with an age short enough that everything fell outside it, and dropping one asset was not possible at all.
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
	settleCLIProject(t, base)

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

	// A listing must not count as using the cache.
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
	settleCLIProject(t, base)

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

// Two coordinates that resolved to the same bytes share one object, so removing one can uncache the other.
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
	settleCLIProject(t, base)

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

// Config merged across the XDG search path raises two questions a person cannot answer by looking: which files was it, and what did they settle on.
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

// A flag beats the config file, which is the whole point of leaving one on the command line.
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
	// The config says one asset at a time; the flag says three.
	assertSuccess(t, runJSON(t, appendArgs(base, "cache", "clear")), "cache.clear")
	assertSuccess(t, runJSON(t, appendArgs(base, "pull", "--concurrency", "3", "--no-progress")), "pull")
}

// TestAddNameCarriesThroughToTheUnpackedFile covers the whole reason --name exists.
func TestAddNameCarriesThroughToTheUnpackedFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Disposition", `attachment; filename="geo-database-2026.08-final.bin"`)
		_, _ = writer.Write([]byte("asset bytes"))
	}))
	defer server.Close()

	paths := newProject(t)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	result := runJSON(t, appendArgs(paths.base, "add", "app/geo@1", server.URL+"/download?id=1234",
		"--name", "geo.db", "--no-progress"))
	assertSuccess(t, result, "add")
	if data := result.value["data"].(map[string]any); data["filename"] != "geo.db" {
		t.Fatalf("the add result reports the file name %v", data["filename"])
	}

	manifest, err := project.ReadManifest(paths.manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if declared := manifest.Assets[coord.MustParse("app/geo@1")].Filename; declared != "geo.db" {
		t.Fatalf("the manifest declares %q", declared)
	}
	settleCLIProject(t, paths.base)

	destination := t.TempDir()
	result = runJSON(t, appendArgs(paths.base, "unpack", "--dest", destination))
	assertSuccess(t, result, "unpack")
	if written, err := os.ReadFile(filepath.Join(destination, "geo.db")); err != nil || string(written) != "asset bytes" {
		t.Fatalf("the unpack wrote %q: %v", written, err)
	}
}

// An add that leaves --name off names the asset exactly as it did before the flag existed, which is the flag's whole compatibility claim.
func TestAddWithoutNameKeepsTheOriginNaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Disposition", `attachment; filename="database.bin"`)
		_, _ = writer.Write([]byte("asset bytes"))
	}))
	defer server.Close()

	paths := newProject(t)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "app/geo@1", server.URL+"/download?id=1234",
		"--no-progress")), "add")
	settleCLIProject(t, paths.base)

	manifest, err := project.ReadManifest(paths.manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if declared := manifest.Assets[coord.MustParse("app/geo@1")].Filename; declared != "" {
		t.Fatalf("an add with no --name declared %q", declared)
	}
	lock, err := project.ReadLock(paths.lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if name := lock.Assets[coord.MustParse("app/geo@1")].Filename; name != "database.bin" {
		t.Fatalf("the lock recorded %q, want the name the origin gave", name)
	}
}

// A name DAC will not write fails the add rather than being repaired into one it will.
func TestAddRefusesANameThatIsNotAFileName(t *testing.T) {
	paths := newProject(t)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	manifestBefore := projecttest.MustRead(t, paths.manifestPath)

	result := runJSON(t, appendArgs(paths.base, "add", "app/geo@1", "https://example.com/geo.bin",
		"--name", "../../etc/passwd", "--offline"))
	assertError(t, result, "invalid_arguments")
	if !bytes.Equal(manifestBefore, projecttest.MustRead(t, paths.manifestPath)) {
		t.Fatal("a refused name still changed the manifest")
	}
}
