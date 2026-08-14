package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/lockfile"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/project"
)

// TestWorkflow exercises the public trust boundary: only the explicit update
// flag accepts current remote bytes, while ordinary pull restores locked bytes.
func TestWorkflowUpdatesLockAndReproducesBytes(t *testing.T) {
	body := []byte("first accepted bytes")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/artifact.bin" {
			http.NotFound(writer, request)
			return
		}
		_, _ = writer.Write(body)
	}))
	defer server.Close()

	withinTempDir(t, func(directory string) {
		runOK(t, "init")
		runOK(t, "add", "artifact", server.URL+"/artifact.bin")
		runOK(t, "pull", "--update-lockfile")

		// Updating the lockfile is the explicit acceptance step: it installs the
		// fetched bytes and records their digest together.
		download := filepath.Join(directory, ".dac", "downloads", "artifact.bin")
		assertFile(t, download, body)
		// Pull repairs local drift from the lock instead of treating whatever is
		// currently on disk as trusted state.
		if err := os.WriteFile(download, []byte("corrupted"), 0o644); err != nil {
			t.Fatal(err)
		}
		runOK(t, "pull")
		assertFile(t, download, body)

		locked, err := os.ReadFile("dac.lock")
		if err != nil {
			t.Fatal(err)
		}
		// --force permits a fresh download, not a digest bypass. If upstream
		// mutates, pull must reject the bytes and preserve the accepted lock.
		body = []byte("different upstream bytes")
		stdout, stderr, code := run("pull", "--force", "--update-lockfile")
		if code != 1 || stdout != "" || !strings.Contains(stderr, "integrity") {
			t.Fatalf("forced changed upstream: code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		current, err := os.ReadFile("dac.lock")
		if err != nil || !bytes.Equal(current, locked) {
			t.Fatalf("lock changed after failed pull: %q, %v", current, err)
		}
	})
}

// TestCalculatedPinDoesNotInstallOrPreacceptBytes keeps pin calculation from
// becoming an implicit lock operation or a hidden local cache.
func TestCalculatedPinDoesNotInstallOrPreacceptBytes(t *testing.T) {
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		_, _ = writer.Write([]byte("pinned bytes"))
	}))
	defer server.Close()

	withinTempDir(t, func(directory string) {
		runOK(t, "init")
		// --pin performs one read to calculate a digest for desired state, but it
		// deliberately creates neither accepted lock metadata nor installed bytes.
		runOK(t, "add", "--pin", "artifact", server.URL+"/artifact.bin")
		if requests != 1 {
			t.Fatalf("pin requests=%d, want 1", requests)
		}
		if _, err := os.Stat("dac.lock"); !os.IsNotExist(err) {
			t.Fatalf("pin created lock: %v", err)
		}
		if _, err := os.Stat(filepath.Join(directory, ".dac", "downloads", "artifact.bin")); !os.IsNotExist(err) {
			t.Fatalf("pin installed file: %v", err)
		}
		paths := project.Paths{Root: directory}
		value, err := manifest.Load(paths.Manifest())
		if err != nil || value.Files["artifact"].Pin == "" {
			t.Fatalf("calculated pin = %#v, %v", value.Files["artifact"], err)
		}
		// Lockfile update must fetch independently rather than reuse preaccepted
		// hidden state from pin calculation.
		runOK(t, "pull", "--update-lockfile")
		if requests != 2 {
			t.Fatalf("lockfile update did not download independently; requests=%d", requests)
		}
	})
}

// TestUpdateMakesLockStaleAndJSONDoesNotLeakQuery covers both sides of a safe
// stale-lock error. Changing a template variable must require relocking because
// it changes the resolved URL, and structured diagnostics must remain valid JSON
// without exposing query-string credentials.
func TestUpdateMakesLockStaleAndJSONDoesNotLeakQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(request.URL.Path))
	}))
	defer server.Close()

	withinTempDir(t, func(string) {
		runOK(t, "init")
		runOK(t, "add", "--set", "VERSION=one", "artifact", server.URL+"/artifact-{{.VERSION}}.bin?token=do-not-print")
		runOK(t, "pull", "--update-lockfile")
		runOK(t, "update", "artifact", "--set", "VERSION=two")
		_, stderr, code := run("pull", "--json")
		if code != 2 || strings.Contains(stderr, "do-not-print") {
			t.Fatalf("stale JSON error: code=%d stderr=%q", code, stderr)
		}
		var output struct {
			Kind string `json:"kind"`
			Hint string `json:"hint"`
		}
		if err := json.Unmarshal([]byte(stderr), &output); err != nil || output.Kind != "configuration" || output.Hint != "run: dac pull --update-lockfile" {
			t.Fatalf("JSON error = %#v, %v", output, err)
		}
	})
}

// TestGlobalVariablesAreSharedAndDefinedOnce covers the whole global-variable
// contract from the command line. One --gset defines a value every asset can
// render through the .global namespace; redefining it is refused because a
// rebind would silently move every asset that references it, and changing it in
// dac.toml deliberately makes those assets' locks stale.
func TestGlobalVariablesAreSharedAndDefinedOnce(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(request.URL.Path))
	}))
	defer server.Close()

	withinTempDir(t, func(directory string) {
		runOK(t, "init")
		// The same command may introduce a global and the artifact using it.
		runOK(t, "add", "--gset", "VERSION=one", "--file", "first-{{.global.VERSION}}.bin",
			"first", server.URL+"/first-{{.global.VERSION}}.bin")
		runOK(t, "add", "--set", "FLAVOUR=beta", "--file", "second-{{.global.VERSION}}-{{.FLAVOUR}}.bin",
			"second", server.URL+"/second-{{.global.VERSION}}.bin")
		runOK(t, "pull", "--update-lockfile")
		assertFile(t, filepath.Join(directory, ".dac", "downloads", "first-one.bin"), []byte("/first-one.bin"))
		assertFile(t, filepath.Join(directory, ".dac", "downloads", "second-one-beta.bin"), []byte("/second-one.bin"))

		// A global is define-once from the CLI, on either command that writes
		// desired state, and the refused edit must leave the manifest alone.
		for _, args := range [][]string{
			{"update", "first", "--gset", "VERSION=two"},
			{"add", "--gset", "VERSION=two", "third", server.URL + "/third.bin"},
		} {
			_, stderr, code := run(args...)
			if code != 2 || !strings.Contains(stderr, "global variable already exists") {
				t.Fatalf("dac %s: code=%d stderr=%q", strings.Join(args, " "), code, stderr)
			}
		}
		value, err := manifest.Load(filepath.Join(directory, "dac.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if value.Globals["VERSION"] != "one" || len(value.Globals) != 1 || len(value.Files) != 2 {
			t.Fatalf("refused global edits changed the manifest: %#v", value)
		}
		runOK(t, "pull")

		// Editing the global is the deliberate path, and it moves both assets,
		// so the lock recorded for each of them no longer describes desired state.
		value.Globals["VERSION"] = "two"
		if err := manifest.Write(filepath.Join(directory, "dac.toml"), value); err != nil {
			t.Fatal(err)
		}
		if _, _, code := run("pull"); code != 2 {
			t.Fatalf("pull after global change code=%d, want stale configuration", code)
		}
	})
}

// TestCrossOriginRedirectStripsConfiguredHeaders proves the header policy at
// the application boundary. A token supplied through an environment-backed
// manifest header may authenticate the origin, but it must not follow a
// redirect to another host; the redirected asset should still be downloadable.
func TestCrossOriginRedirectStripsConfiguredHeaders(t *testing.T) {
	var leaked string
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		leaked = request.Header.Get("Authorization")
		_, _ = writer.Write([]byte("redirected"))
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/artifact.bin", http.StatusFound)
	}))
	defer origin.Close()

	withinTempDir(t, func(directory string) {
		runOK(t, "init")
		manifest := "version = 1\n\n[files.artifact]\nurl = \"" + origin.URL + "/artifact.bin\"\nfile = \"artifact.bin\"\n\n[files.artifact.headers]\nAuthorization = \"env:TEST_DAC_TOKEN\"\n"
		if err := os.WriteFile("dac.toml", []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TEST_DAC_TOKEN", "secret")
		runOK(t, "pull", "--update-lockfile")
		if leaked != "" {
			t.Fatalf("cross-origin Authorization leaked as %q", leaked)
		}
		assertFile(t, filepath.Join(directory, ".dac", "downloads", "artifact.bin"), []byte("redirected"))
	})
}

// TestOpaqueAssetNamesAndControlValuesRoundTrip exercises the CLI-to-manifest
// representation boundary. Names containing namespace punctuation, shell
// metacharacters, or a leading dash must remain opaque, while non-printing
// variable values must survive TOML serialization without corruption.
func TestOpaqueAssetNamesAndControlValuesRoundTrip(t *testing.T) {
	withinTempDir(t, func(directory string) {
		runOK(t, "init")
		runOK(t, "add", "--set", "UNUSED=\v", "--file", "artifact.bin", "namespace/asset@version", "https://example.com/artifact.bin")
		runOK(t, "add", "--file", "leading.bin", "--", "-leading'$`", "https://example.com/leading.bin")
		paths := project.Paths{Root: directory}
		value, err := manifest.Load(paths.Manifest())
		if err != nil {
			t.Fatal(err)
		}
		if value.Files["namespace/asset@version"].Variables["UNUSED"] != "\v" {
			t.Fatalf("control-valued variable did not round trip: %#v", value.Files)
		}
		if _, exists := value.Files["-leading'$`"]; !exists {
			t.Fatalf("leading-dash asset was not persisted: %#v", value.Files)
		}
	})
}

// TestAddRejectsUnicodeEquivalentDestination exposes filename collision checks
// through the CLI. Visually identical composed and decomposed Unicode names can
// alias on disk, so adding the second asset must fail before the manifest can
// describe an unsafe installation.
func TestAddRejectsUnicodeEquivalentDestination(t *testing.T) {
	withinTempDir(t, func(string) {
		runOK(t, "init")
		runOK(t, "add", "--file", "é.bin", "first", "https://example.com/first")
		_, stderr, code := run("add", "--file", "e\u0301.bin", "second", "https://example.com/second")
		if code != 2 || !strings.Contains(stderr, "conflicts") {
			t.Fatalf("Unicode collision: code=%d stderr=%q", code, stderr)
		}
	})
}

// TestLockfileUpdateRejectsDownloadsSymlinkBeforeNetworkAccess checks both
// filesystem confinement and operation ordering. DAC must validate its download
// root before performing a request that could never commit safely.
func TestLockfileUpdateRejectsDownloadsSymlinkBeforeNetworkAccess(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		_, _ = writer.Write([]byte("outside"))
	}))
	defer server.Close()

	withinTempDir(t, func(directory string) {
		runOK(t, "init")
		runOK(t, "add", "artifact", server.URL+"/artifact.bin")
		downloads := filepath.Join(directory, ".dac", "downloads")
		if err := os.Remove(downloads); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		if err := os.Symlink(outside, downloads); err != nil {
			t.Fatal(err)
		}
		_, _, code := run("pull", "--update-lockfile")
		if code != 1 {
			t.Fatalf("pull update code=%d, want filesystem failure", code)
		}
		if requests != 0 {
			t.Fatalf("pull update made %d HTTP requests before rejecting the symlink", requests)
		}
		entries, err := os.ReadDir(outside)
		if err != nil || len(entries) != 0 {
			t.Fatalf("outside directory changed: %v, %v", entries, err)
		}
	})
}

// TestPullUpdateRollsBackDownloadsWhenLockfileCommitFails protects the update's
// all-or-nothing transaction. Accepted metadata and bytes must change together.
func TestPullUpdateRollsBackDownloadsWhenLockfileCommitFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := os.Mkdir("dac.lock", 0o755); err != nil && !os.IsExist(err) {
			t.Errorf("create lockfile blocker: %v", err)
		}
		_, _ = writer.Write([]byte("staged bytes"))
	}))
	defer server.Close()

	withinTempDir(t, func(directory string) {
		runOK(t, "init")
		runOK(t, "add", "artifact", server.URL+"/artifact.bin")
		_, _, code := run("pull", "--update-lockfile")
		if code == 0 {
			t.Fatal("pull update succeeded after dac.lock was replaced concurrently")
		}
		if _, err := os.Stat(filepath.Join(directory, ".dac", "downloads", "artifact.bin")); !os.IsNotExist(err) {
			t.Fatalf("download survived failed lock transaction: %v", err)
		}
	})
}

func TestInitRejectsConflictingOutputModesBeforeCreatingProject(t *testing.T) {
	withinTempDir(t, func(string) {
		_, stderr, code := run("--json", "--quiet", "init")
		if code != 2 || !strings.Contains(stderr, "--json and --quiet") {
			t.Fatalf("init conflict: code=%d stderr=%q", code, stderr)
		}
		if _, err := os.Stat("dac.toml"); !os.IsNotExist(err) {
			t.Fatalf("invalid init created manifest: %v", err)
		}
	})
}

func TestScopedUpdateBootstrapsAndRejectsLegacyLock(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = writer.Write([]byte("locked bytes"))
	}))
	defer server.Close()

	withinTempDir(t, func(string) {
		runOK(t, "init")
		runOK(t, "add", "artifact", server.URL+"/artifact.bin")
		runOK(t, "pull", "artifact", "--update-lockfile")
		if requests != 1 {
			t.Fatalf("scoped bootstrap requests=%d, want 1", requests)
		}
		before, err := os.ReadFile("dac.lock")
		if err != nil {
			t.Fatal(err)
		}
		runOK(t, "pull", "artifact", "--update-lockfile")
		if requests != 1 {
			t.Fatalf("current update made an HTTP request; requests=%d", requests)
		}
		if after, err := os.ReadFile("dac.lock"); err != nil || !bytes.Equal(after, before) {
			t.Fatalf("current update rewrote dac.lock: %q, %v", after, err)
		}

		for name, invalid := range map[string]string{
			"legacy":    `{"version":1,"files":{}}` + "\n",
			"malformed": "{not-json\n",
		} {
			if err := os.WriteFile("dac.lock", []byte(invalid), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, _, code := run("pull"); code != 2 {
				t.Fatalf("pull with %s lock code=%d, want 2", name, code)
			}
			if _, _, code := run("pull", "artifact", "--update-lockfile"); code != 2 {
				t.Fatalf("scoped update with %s lock code=%d, want 2", name, code)
			}
			if requests != 1 {
				t.Fatalf("%s lock triggered a request; requests=%d", name, requests)
			}
			if current, err := os.ReadFile("dac.lock"); err != nil || string(current) != invalid {
				t.Fatalf("%s lock changed: %q, %v", name, current, err)
			}
		}
	})
}

func TestHelpExposesScopedPullWithoutRemovedCommands(t *testing.T) {
	stdout, stderr, code := run("--help")
	if code != 0 || stderr != "" || strings.Contains(stdout, "\n  lock ") || strings.Contains(stdout, "\n  status ") {
		t.Fatalf("top-level help: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = run("pull", "--help")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "pull [flags] [names...]") || !strings.Contains(stdout, "--update-lockfile") {
		t.Fatalf("pull help: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

// TestScopedPullIgnoresUnselectedStaleState proves that a name is a real
// operation boundary rather than only an output filter.
func TestScopedPullIgnoresUnselectedStaleState(t *testing.T) {
	var firstRequests, secondRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/first.bin":
			firstRequests.Add(1)
		case "/second.bin":
			secondRequests.Add(1)
		}
		_, _ = writer.Write([]byte(request.URL.Path))
	}))
	defer server.Close()

	withinTempDir(t, func(directory string) {
		runOK(t, "init")
		runOK(t, "add", "first", server.URL+"/first.bin")
		runOK(t, "add", "second", server.URL+"/second.bin")
		runOK(t, "pull", "--update-lockfile")
		runOK(t, "update", "second", "--set", "VERSION=two")
		if err := os.Remove(filepath.Join(directory, ".dac", "downloads", "first.bin")); err != nil {
			t.Fatal(err)
		}

		runOK(t, "pull", "first")
		if firstRequests.Load() != 2 || secondRequests.Load() != 1 {
			t.Fatalf("scoped requests = (%d, %d), want (2, 1)", firstRequests.Load(), secondRequests.Load())
		}
		if _, _, code := run("pull"); code != 2 {
			t.Fatalf("bare pull with unselected stale asset code=%d, want 2", code)
		}
		beforeFirst, beforeSecond := firstRequests.Load(), secondRequests.Load()
		for _, args := range [][]string{{"pull", "missing"}, {"pull", "first", "first"}} {
			if _, _, code := run(args...); code != 2 {
				t.Fatalf("dac %s code=%d, want 2", strings.Join(args, " "), code)
			}
		}
		if firstRequests.Load() != beforeFirst || secondRequests.Load() != beforeSecond {
			t.Fatal("invalid scope made a network request")
		}
	})
}

// TestScopedBootstrapCreatesPartialLock pins the intentional incomplete-lock
// workflow: later scoped pulls work, while a bare pull still enforces complete
// accepted state.
func TestScopedBootstrapCreatesPartialLock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(request.URL.Path))
	}))
	defer server.Close()

	withinTempDir(t, func(string) {
		runOK(t, "init")
		runOK(t, "add", "first", server.URL+"/first.bin")
		runOK(t, "add", "second", server.URL+"/second.bin")
		runOK(t, "pull", "first", "--update-lockfile")
		locked, err := lockfile.Load("dac.lock")
		if err != nil || len(locked.Files) != 1 || locked.Files["first"].Digest == "" {
			t.Fatalf("partial lock = %#v, %v", locked, err)
		}
		runOK(t, "pull", "first")
		if _, _, code := run("pull"); code != 2 {
			t.Fatalf("bare partial pull code=%d, want 2", code)
		}
		_, stderr, code := run("pull", "second", "--json")
		if code != 2 || !strings.Contains(stderr, "dac pull --update-lockfile -- 'second'") {
			t.Fatalf("missing scoped entry: code=%d stderr=%q", code, stderr)
		}
		runOK(t, "pull", "second", "--update-lockfile")
		runOK(t, "pull")
	})
}

// TestScopedUpdatePreservesUnselectedStateAndBareUpdateCleansIt verifies both
// ownership boundaries for lock-only entries and stale manifest assets.
func TestScopedUpdatePreservesUnselectedStateAndBareUpdateCleansIt(t *testing.T) {
	var firstRequests, secondRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/first.bin" {
			firstRequests.Add(1)
		} else {
			secondRequests.Add(1)
		}
		_, _ = writer.Write([]byte(request.URL.Path))
	}))
	defer server.Close()

	withinTempDir(t, func(directory string) {
		runOK(t, "init")
		runOK(t, "add", "first", server.URL+"/first.bin")
		runOK(t, "add", "second", server.URL+"/second.bin")
		runOK(t, "pull", "--update-lockfile")
		locked, err := lockfile.Load("dac.lock")
		if err != nil {
			t.Fatal(err)
		}
		secondBefore := locked.Files["second"]
		locked.Files["orphan"] = lockfile.Asset{
			ResolvedURL: "https://example.com/orphan", ResolvedFile: "orphan.bin",
			ConfigurationDigest: "sha256:" + strings.Repeat("1", 64), Digest: digestFor([]byte("orphan")), Size: 6,
		}
		writeTestLock(t, "dac.lock", locked)
		orphanPath := filepath.Join(directory, ".dac", "downloads", "orphan.bin")
		if err := os.WriteFile(orphanPath, []byte("orphan"), 0o644); err != nil {
			t.Fatal(err)
		}
		runOK(t, "update", "first", "--set", "VERSION=two")
		runOK(t, "update", "second", "--set", "VERSION=two")

		runOK(t, "pull", "first", "--update-lockfile")
		afterScoped, err := lockfile.Load("dac.lock")
		if err != nil || afterScoped.Files["second"] != secondBefore {
			t.Fatalf("scoped update changed second: %#v, %v", afterScoped.Files["second"], err)
		}
		if _, exists := afterScoped.Files["orphan"]; !exists {
			t.Fatal("scoped update removed lock-only entry")
		}
		if firstRequests.Load() != 2 || secondRequests.Load() != 1 {
			t.Fatalf("scoped update requests = (%d, %d), want (2, 1)", firstRequests.Load(), secondRequests.Load())
		}
		assertFile(t, orphanPath, []byte("orphan"))

		// Explicitly naming every manifest asset is still scoped and therefore
		// does not claim ownership of lock-only entries.
		runOK(t, "pull", "first", "second", "--update-lockfile")
		afterNamedAll, err := lockfile.Load("dac.lock")
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := afterNamedAll.Files["orphan"]; !exists {
			t.Fatal("explicit all-asset scope removed lock-only entry")
		}
		if secondRequests.Load() != 2 {
			t.Fatalf("named update second requests=%d, want 2", secondRequests.Load())
		}

		runOK(t, "pull", "--update-lockfile")
		afterFull, err := lockfile.Load("dac.lock")
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := afterFull.Files["orphan"]; exists {
			t.Fatal("bare update preserved lock-only entry")
		}
		if _, err := os.Stat(orphanPath); !os.IsNotExist(err) {
			t.Fatalf("bare update preserved orphan download: %v", err)
		}
		if firstRequests.Load() != 2 || secondRequests.Load() != 2 {
			t.Fatalf("bare orphan cleanup made requests = (%d, %d)", firstRequests.Load(), secondRequests.Load())
		}
	})
}

func TestPullRejectsOfflineLockfileUpdate(t *testing.T) {
	_, stderr, code := run("pull", "--offline", "--update-lockfile")
	if code != 2 || !strings.Contains(stderr, "--offline and --update-lockfile") {
		t.Fatalf("offline update: code=%d stderr=%q", code, stderr)
	}
}

func TestUpdateEditsCompleteAssetPolicyAndRejectsNoOps(t *testing.T) {
	body := []byte("policy bytes")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(body)
	}))
	defer server.Close()

	withinTempDir(t, func(directory string) {
		runOK(t, "init")
		runOK(t, "add",
			"--set", "VERSION=one", "--set", "UNUSED=value",
			"--file", "old-{{.VERSION}}.bin", "--header", "X-Old=value",
			"--pin="+digestFor(body), "--max-size", "1MiB", "--idle-timeout", "2s",
			"artifact", server.URL+"/old-{{.VERSION}}",
		)
		runOK(t, "pull", "--update-lockfile")
		runOK(t, "update", "artifact",
			"--url", server.URL+"/new-{{.VERSION}}", "--file", "new-{{.VERSION}}.bin",
			"--set", "VERSION=two", "--unset", "UNUSED",
			"--header", "X-New=new", "--remove-header", "x-old", "--unpin",
			"--max-size", "2MiB", "--idle-timeout", "4s",
		)
		value, err := manifest.Load(filepath.Join(directory, "dac.toml"))
		if err != nil {
			t.Fatal(err)
		}
		file := value.Files["artifact"]
		if file.URL != server.URL+"/new-{{.VERSION}}" || file.File != "new-{{.VERSION}}.bin" ||
			file.Variables["VERSION"] != "two" || file.Variables["UNUSED"] != "" || file.Pin != "" ||
			file.Headers["X-New"] != "new" || len(file.Headers) != 1 || file.MaxSize != "2MiB" || file.IdleTimeout != "4s" {
			t.Fatalf("updated asset = %#v", file)
		}
		if _, _, code := run("pull"); code != 2 {
			t.Fatalf("pull after policy update code=%d, want stale configuration", code)
		}
		if _, stderr, code := run("update", "artifact", "--set", "VERSION=two"); code != 2 || !strings.Contains(stderr, "no changes") {
			t.Fatalf("no-op update: code=%d stderr=%q", code, stderr)
		}
		if _, _, code := run("update", "artifact", "--unset", "MISSING"); code != 2 {
			t.Fatalf("unknown variable removal code=%d, want 2", code)
		}
		if _, _, code := run("update", "artifact", "--remove-header", "Missing"); code != 2 {
			t.Fatalf("unknown header removal code=%d, want 2", code)
		}
	})
}

func TestScopedUpdateRemovesOnlyPreviouslyManagedObsoleteFile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("content"))
	}))
	defer server.Close()

	withinTempDir(t, func(directory string) {
		runOK(t, "init")
		runOK(t, "add", "--file", "old.bin", "artifact", server.URL+"/artifact")
		runOK(t, "pull", "--update-lockfile")
		downloads := filepath.Join(directory, ".dac", "downloads")
		extra := filepath.Join(downloads, "extra.bin")
		if err := os.WriteFile(extra, []byte("unmanaged"), 0o644); err != nil {
			t.Fatal(err)
		}
		runOK(t, "update", "artifact", "--file", "new.bin")
		runOK(t, "pull", "artifact", "--update-lockfile")
		if _, err := os.Stat(filepath.Join(downloads, "old.bin")); !os.IsNotExist(err) {
			t.Fatalf("obsolete managed file remains: %v", err)
		}
		assertFile(t, filepath.Join(downloads, "new.bin"), []byte("content"))
		assertFile(t, extra, []byte("unmanaged"))
	})
}

// TestConcurrentDownloadsOverlapAndJobsOneDoesNot pins both halves of dac's
// transfer concurrency: several assets are fetched at the same time by default,
// and --jobs 1 still means one request in flight for callers who need a host to
// see exactly one client.
func TestConcurrentDownloadsOverlapAndJobsOneDoesNot(t *testing.T) {
	const assets = 3
	// While the rendezvous is armed each request waits for the others to
	// arrive, so a handler can only answer if dac really does have three
	// transfers open at once.
	together := make(chan struct{})
	var rendezvous atomic.Bool
	var inFlight atomic.Int64
	var peak atomic.Int64
	rendezvous.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		current := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			highest := peak.Load()
			if current <= highest || peak.CompareAndSwap(highest, current) {
				break
			}
		}
		if rendezvous.Load() {
			if current == assets {
				close(together)
			}
			select {
			case <-together:
			case <-request.Context().Done():
			case <-time.After(10 * time.Second):
			}
		}
		_, _ = writer.Write([]byte(request.URL.Path))
	}))
	defer server.Close()

	withinTempDir(t, func(string) {
		runOK(t, "init")
		for index := range assets {
			name := fmt.Sprintf("artifact-%d", index)
			runOK(t, "add", name, fmt.Sprintf("%s/%s.bin", server.URL, name))
		}
		runOK(t, "pull", "--update-lockfile")
		if peak.Load() != assets {
			t.Fatalf("lockfile update ran %d transfers at once, want %d", peak.Load(), assets)
		}

		// One transfer at a time can never satisfy the rendezvous, so the second
		// half measures overlap against a handler that answers immediately.
		rendezvous.Store(false)
		peak.Store(0)
		runOK(t, "pull", "--force", "--jobs", "1")
		if peak.Load() != 1 {
			t.Fatalf("--jobs 1 ran %d transfers at once, want 1", peak.Load())
		}
	})
}

// TestFailedConcurrentUpdateStagesNothing keeps concurrency out of accepted
// state: bytes that arrive beside a failing asset must remain staged.
func TestFailedConcurrentUpdateStagesNothing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "broken") {
			http.Error(writer, "gone", http.StatusGone)
			return
		}
		_, _ = writer.Write([]byte(request.URL.Path))
	}))
	defer server.Close()

	withinTempDir(t, func(directory string) {
		runOK(t, "init")
		runOK(t, "add", "healthy", server.URL+"/healthy.bin")
		runOK(t, "add", "broken", server.URL+"/broken.bin")
		if _, _, code := run("pull", "--update-lockfile"); code != 1 {
			t.Fatalf("pull update code=%d, want a network failure", code)
		}
		if _, err := os.Stat("dac.lock"); !os.IsNotExist(err) {
			t.Fatalf("failed update wrote dac.lock: %v", err)
		}
		entries, err := os.ReadDir(filepath.Join(directory, ".dac", "downloads"))
		if err != nil || len(entries) != 0 {
			t.Fatalf("failed update left %v behind: %v", entries, err)
		}
	})
}

// TestJobsRejectsValuesDACWillNotRun keeps the concurrency option honest: it is
// an invalid invocation rather than a number dac quietly reinterprets.
func TestJobsRejectsValuesDACWillNotRun(t *testing.T) {
	withinTempDir(t, func(string) {
		runOK(t, "init")
		for _, value := range []string{"0", "-1", "99", "many"} {
			if _, _, code := run("pull", "--jobs", value); code != 2 {
				t.Fatalf("--jobs %s code=%d, want 2", value, code)
			}
		}
		if _, _, code := run("pull", "--jobs", "2", "--jobs", "3"); code != 2 {
			t.Fatal("repeated --jobs was accepted")
		}
	})
}

func digestFor(value []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(value))
}

func writeTestLock(t *testing.T, path string, value lockfile.Lockfile) {
	t.Helper()
	file, err := lockfile.Stage(path, value)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Discard() }()
	if err := file.Commit(); err != nil {
		t.Fatal(err)
	}
}

// withinTempDir isolates CLI tests that resolve project files from the process
// working directory. Tests using it must not call t.Parallel because os.Chdir is
// process-global; cleanup always restores the developer's original directory.
func withinTempDir(t *testing.T, work func(string)) {
	t.Helper()
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.Chdir(directory); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })
	work(directory)
}

// runOK executes DAC and reports all three observable channels when a command
// expected to succeed fails, keeping workflow-test failures diagnosable.
func runOK(t *testing.T, args ...string) {
	t.Helper()
	stdout, stderr, code := run(args...)
	if code != 0 {
		t.Fatalf("dac %s: code=%d stdout=%q stderr=%q", strings.Join(args, " "), code, stdout, stderr)
	}
}

// run invokes the application boundary in-process so tests can assert stdout,
// stderr, and exit-code policy without spawning a platform-dependent binary.
func run(args ...string) (string, string, int) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := Run(context.Background(), args, stdout, stderr)
	return stdout.String(), stderr.String(), code
}

// assertFile compares exact bytes because DAC's core promise is reproducibility;
// semantically similar or normalized content is still an integrity failure.
func assertFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
	}
}

// TestExitCodeSeparatesCallerFromOperatorFailures pins the process contract now
// that exit-status policy lives in main rather than in the output writer. A
// configuration failure is the caller's to fix and exits 2; an integrity
// failure is not, and exits 1.
func TestExitCodeSeparatesCallerFromOperatorFailures(t *testing.T) {
	body := []byte("locked bytes")
	serve := body
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write(serve)
	}))
	defer server.Close()

	withinTempDir(t, func(directory string) {
		runOK(t, "init")
		runOK(t, "add", "artifact", server.URL+"/artifact.bin")
		runOK(t, "pull", "--update-lockfile")

		// A stale lock is a configuration failure: the caller edits dac.toml.
		runOK(t, "update", "artifact", "--set", "VERSION=two")
		if _, _, code := run("pull"); code != 2 {
			t.Fatalf("stale pull code=%d, want 2", code)
		}
		runOK(t, "pull", "--update-lockfile")

		// Bytes that no longer match the accepted digest are an integrity
		// failure: nothing the caller can correct by editing input.
		serve = []byte("different bytes entirely")
		if err := os.Remove(filepath.Join(directory, ".dac", "downloads", "artifact.bin")); err != nil {
			t.Fatal(err)
		}
		_, stderr, code := run("pull")
		if code != 1 {
			t.Fatalf("integrity pull code=%d stderr=%q, want 1", code, stderr)
		}
	})
}

// TestRecoveryHintRendersAsAPasteableCommand proves the structured recovery
// survives the trip from the library packages to the process boundary. The
// asset name is opaque and shell-hostile, so it must arrive quoted.
func TestRecoveryHintRendersAsAPasteableCommand(t *testing.T) {
	const name = "-scope/pkg'$`"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("locked bytes"))
	}))
	defer server.Close()

	withinTempDir(t, func(string) {
		runOK(t, "init")
		runOK(t, "add", "--", name, server.URL+"/artifact.bin")
		runOK(t, "pull", "--update-lockfile")
		runOK(t, "update", "--set", "VERSION=two", "--", name)

		_, stderr, code := run("pull", "--json", "--", name)
		if code != 2 {
			t.Fatalf("stale pull code=%d stderr=%q, want 2", code, stderr)
		}
		var output struct {
			Hint string `json:"hint"`
		}
		if err := json.Unmarshal([]byte(stderr), &output); err != nil {
			t.Fatal(err)
		}
		want := `run: dac pull --update-lockfile -- '-scope/pkg'"'"'$` + "`'"
		if output.Hint != want {
			t.Fatalf("hint = %q, want %q", output.Hint, want)
		}
	})
}
