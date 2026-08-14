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
		runOK(t, "add", server.URL+"/artifact.bin")
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
		runOK(t, "add", server.URL+"/{{.VERSION}}/artifact.bin?token=do-not-print")
		runOK(t, "update", "artifact.bin", "--set", "VERSION=one")
		runOK(t, "pull", "--update-lockfile")
		runOK(t, "update", "artifact.bin", "--set", "VERSION=two")
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

// TestGroupedReleaseVariablesDriveBatchAssets covers the primary application
// workflow: add unresolved URLs together, assign their release coordinates,
// and explicitly accept all resulting bytes in one pull.
func TestGroupedReleaseVariablesDriveBatchAssets(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		_, _ = writer.Write([]byte(request.URL.Path))
	}))
	defer server.Close()

	withinTempDir(t, func(directory string) {
		runOK(t, "init")
		runOK(t, "add",
			server.URL+"/curam/{{$.VERSION}}/Curam.ear",
			server.URL+"/rest/{{$.VERSION}}/rest.ear",
			server.URL+"/services/{{$.SERVICE_VERSION}}/CuramServices.ear",
		)
		if requests.Load() != 0 {
			t.Fatalf("add made %d requests", requests.Load())
		}
		// The first assignment persists even while another referenced release
		// remains unset, but a bare pull still performs no network activity.
		runOK(t, "update", "--set", "$.VERSION=1.2.3")
		if _, _, code := run("pull", "--update-lockfile"); code != 2 || requests.Load() != 0 {
			t.Fatalf("unresolved pull code=%d requests=%d", code, requests.Load())
		}
		runOK(t, "pull", "Curam.ear", "--update-lockfile")
		if requests.Load() != 1 {
			t.Fatalf("scoped ready pull requests=%d, want 1", requests.Load())
		}
		runOK(t, "update", "--set", "$.SERVICE_VERSION=1.2.2")
		runOK(t, "pull", "--update-lockfile")
		assertFile(t, filepath.Join(directory, ".dac", "downloads", "Curam.ear"), []byte("/curam/1.2.3/Curam.ear"))
		assertFile(t, filepath.Join(directory, ".dac", "downloads", "rest.ear"), []byte("/rest/1.2.3/rest.ear"))
		assertFile(t, filepath.Join(directory, ".dac", "downloads", "CuramServices.ear"), []byte("/services/1.2.2/CuramServices.ear"))

		value, err := manifest.Load(filepath.Join(directory, "dac.toml"))
		if err != nil || value.Globals["VERSION"] != "1.2.3" || value.Globals["SERVICE_VERSION"] != "1.2.2" || len(value.Files) != 3 {
			t.Fatalf("group manifest = %#v, %v", value, err)
		}
		if _, stderr, code := run("update", "--set", "$.MISSING=two"); code != 2 || !strings.Contains(stderr, "does not exist") {
			t.Fatalf("unreferenced global: code=%d stderr=%q", code, stderr)
		}
		if _, stderr, code := run("update", "--set", "$.VERSION=1.2.3"); code != 2 || !strings.Contains(stderr, "no changes") {
			t.Fatalf("no-op global update: code=%d stderr=%q", code, stderr)
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
		manifest := "version = 2\n\n[files.artifact]\nurl = \"" + origin.URL + "/artifact.bin\"\nfile = \"artifact.bin\"\n\n[files.artifact.headers]\nAuthorization = \"env:TEST_DAC_TOKEN\"\n"
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
// TestAddRejectsUnicodeEquivalentDestination exposes filename collision checks
// through the CLI. Visually identical composed and decomposed Unicode names can
// alias on disk, so adding the second asset must fail before the manifest can
// describe an unsafe installation.
func TestAddRejectsUnicodeEquivalentDestination(t *testing.T) {
	withinTempDir(t, func(string) {
		runOK(t, "init")
		_, stderr, code := run("add", "https://example.com/é.bin", "https://example.com/e\u0301.bin")
		if code != 2 || !strings.Contains(stderr, "conflicts") {
			t.Fatalf("Unicode collision: code=%d stderr=%q", code, stderr)
		}
		value, err := manifest.Load("dac.toml")
		if err != nil || len(value.Files) != 0 {
			t.Fatalf("failed batch changed manifest: %#v, %v", value.Files, err)
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
		runOK(t, "add", server.URL+"/artifact.bin")
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
		runOK(t, "add", server.URL+"/artifact.bin")
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
		runOK(t, "add", server.URL+"/artifact.bin")
		runOK(t, "pull", "artifact.bin", "--update-lockfile")
		if requests != 1 {
			t.Fatalf("scoped bootstrap requests=%d, want 1", requests)
		}
		before, err := os.ReadFile("dac.lock")
		if err != nil {
			t.Fatal(err)
		}
		runOK(t, "pull", "artifact.bin", "--update-lockfile")
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
			if _, _, code := run("pull", "artifact.bin", "--update-lockfile"); code != 2 {
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
	stdout, stderr, code = run("add", "--help")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "add <urls...>") || strings.Contains(stdout, "--option") || strings.Contains(stdout, "--def") || strings.Contains(stdout, "--header") {
		t.Fatalf("add help: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = run("update", "--help")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "update [flags] [name]") || !strings.Contains(stdout, "--set") ||
		strings.Contains(stdout, "--option") || strings.Contains(stdout, "--def") || strings.Contains(stdout, "--unset") || strings.Contains(stdout, "--header") {
		t.Fatalf("update help: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestRemovedFlagsAreRejected(t *testing.T) {
	tests := [][]string{
		{"add", "--set", "VERSION=one", "https://example.com/artifact"},
		{"add", "--def", "VERSION=one", "https://example.com/artifact"},
		{"add", "--option", "pin=calculate", "https://example.com/artifact"},
		{"add", "--header", "X-Test=value", "https://example.com/artifact"},
		{"update", "--gset", "VERSION=one"},
		{"update", "--def", "VERSION=one"},
		{"update", "--unset", "VERSION"},
		{"update", "--option", "url=value"},
		{"update", "--unset-option", "pin"},
		{"update", "--header", "X-Test=value"},
		{"update", "--remove-header", "X-Test"},
		{"update", "artifact", "--url", "https://example.com/artifact"},
		{"update", "artifact", "--unpin"},
		{"update", "artifact", "--max-size", "1MiB"},
		{"update", "artifact", "--idle-timeout", "1m"},
	}
	for _, args := range tests {
		if _, stderr, code := run(args...); code != 2 || !strings.Contains(stderr, "unknown flag") {
			t.Fatalf("removed flag %q: code=%d stderr=%q", args, code, stderr)
		}
	}
}

func TestRemovedNamedAddAndTemplatedFilenameAreRejected(t *testing.T) {
	withinTempDir(t, func(string) {
		runOK(t, "init")
		for _, args := range [][]string{
			{"add", "artifact", "https://example.com/artifact.bin"},
			{"add", "https://example.com/artifact-{{$.VERSION}}.bin"},
		} {
			if _, _, code := run(args...); code != 2 {
				t.Fatalf("removed add form %q code=%d, want 2", args, code)
			}
		}
		value, err := manifest.Load("dac.toml")
		if err != nil || len(value.Files) != 0 {
			t.Fatalf("rejected add changed manifest: %#v, %v", value.Files, err)
		}
	})
}

func TestUpdateScopesCannotBeMixed(t *testing.T) {
	if _, stderr, code := run("update", "--set", "VERSION=one"); code != 2 || !strings.Contains(stderr, "requires $.KEY") {
		t.Fatalf("unscoped project update: code=%d stderr=%q", code, stderr)
	}
	if _, stderr, code := run("update", "artifact", "--set", "$.VERSION=one"); code != 2 || !strings.Contains(stderr, "requires unscoped KEY") {
		t.Fatalf("scoped global update: code=%d stderr=%q", code, stderr)
	}
}

func TestInvalidVariableResolutionRollsBackManifest(t *testing.T) {
	withinTempDir(t, func(string) {
		runOK(t, "init")
		value := manifest.Manifest{Version: manifest.Version, Files: map[string]manifest.Asset{
			"artifact.bin": {URL: "https://{{.HOST}}/artifact.bin", File: "artifact.bin"},
		}}
		if err := manifest.Write("dac.toml", value); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile("dac.toml")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, code := run("update", "artifact.bin", "--set", "HOST=bad host"); code != 2 {
			t.Fatalf("invalid resolution code=%d, want 2", code)
		}
		after, err := os.ReadFile("dac.toml")
		if err != nil || !bytes.Equal(after, before) {
			t.Fatalf("failed update changed manifest: %q, %v", after, err)
		}
	})
}

// TestScopedPullIgnoresUnselectedStaleState proves that a name is a real
// operation boundary rather than only an output filter.
func TestScopedPullIgnoresUnselectedStaleState(t *testing.T) {
	var firstRequests, secondRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/first.bin"):
			firstRequests.Add(1)
		case strings.HasSuffix(request.URL.Path, "/second.bin"):
			secondRequests.Add(1)
		}
		_, _ = writer.Write([]byte(request.URL.Path))
	}))
	defer server.Close()

	withinTempDir(t, func(directory string) {
		runOK(t, "init")
		runOK(t, "add", server.URL+"/first.bin", server.URL+"/{{.VERSION}}/second.bin")
		runOK(t, "update", "second.bin", "--set", "VERSION=one")
		runOK(t, "pull", "--update-lockfile")
		runOK(t, "update", "second.bin", "--set", "VERSION=two")
		if err := os.Remove(filepath.Join(directory, ".dac", "downloads", "first.bin")); err != nil {
			t.Fatal(err)
		}

		runOK(t, "pull", "first.bin")
		if firstRequests.Load() != 2 || secondRequests.Load() != 1 {
			t.Fatalf("scoped requests = (%d, %d), want (2, 1)", firstRequests.Load(), secondRequests.Load())
		}
		if _, _, code := run("pull"); code != 2 {
			t.Fatalf("bare pull with unselected stale asset code=%d, want 2", code)
		}
		beforeFirst, beforeSecond := firstRequests.Load(), secondRequests.Load()
		for _, args := range [][]string{{"pull", "missing"}, {"pull", "first.bin", "first.bin"}} {
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
		runOK(t, "add", server.URL+"/first.bin", server.URL+"/second.bin")
		runOK(t, "pull", "first.bin", "--update-lockfile")
		locked, err := lockfile.Load("dac.lock")
		if err != nil || len(locked.Files) != 1 || locked.Files["first.bin"].Digest == "" {
			t.Fatalf("partial lock = %#v, %v", locked, err)
		}
		runOK(t, "pull", "first.bin")
		if _, _, code := run("pull"); code != 2 {
			t.Fatalf("bare partial pull code=%d, want 2", code)
		}
		_, stderr, code := run("pull", "second.bin", "--json")
		if code != 2 || !strings.Contains(stderr, "dac pull --update-lockfile -- 'second.bin'") {
			t.Fatalf("missing scoped entry: code=%d stderr=%q", code, stderr)
		}
		runOK(t, "pull", "second.bin", "--update-lockfile")
		runOK(t, "pull")
	})
}

// TestScopedUpdatePreservesUnselectedStateAndBareUpdateCleansIt verifies both
// ownership boundaries for lock-only entries and stale manifest assets.
func TestScopedUpdatePreservesUnselectedStateAndBareUpdateCleansIt(t *testing.T) {
	var firstRequests, secondRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.HasSuffix(request.URL.Path, "/first.bin") {
			firstRequests.Add(1)
		} else {
			secondRequests.Add(1)
		}
		_, _ = writer.Write([]byte(request.URL.Path))
	}))
	defer server.Close()

	withinTempDir(t, func(directory string) {
		runOK(t, "init")
		runOK(t, "add", server.URL+"/{{.VERSION}}/first.bin", server.URL+"/{{.VERSION}}/second.bin")
		runOK(t, "update", "first.bin", "--set", "VERSION=one")
		runOK(t, "update", "second.bin", "--set", "VERSION=one")
		runOK(t, "pull", "--update-lockfile")
		locked, err := lockfile.Load("dac.lock")
		if err != nil {
			t.Fatal(err)
		}
		secondBefore := locked.Files["second.bin"]
		locked.Files["orphan"] = lockfile.Asset{
			ResolvedURL: "https://example.com/orphan", ResolvedFile: "orphan.bin",
			ConfigurationDigest: "sha256:" + strings.Repeat("1", 64), Digest: digestFor([]byte("orphan")), Size: 6,
		}
		writeTestLock(t, "dac.lock", locked)
		orphanPath := filepath.Join(directory, ".dac", "downloads", "orphan.bin")
		if err := os.WriteFile(orphanPath, []byte("orphan"), 0o644); err != nil {
			t.Fatal(err)
		}
		runOK(t, "update", "first.bin", "--set", "VERSION=two")
		runOK(t, "update", "second.bin", "--set", "VERSION=two")

		runOK(t, "pull", "first.bin", "--update-lockfile")
		afterScoped, err := lockfile.Load("dac.lock")
		if err != nil || afterScoped.Files["second.bin"] != secondBefore {
			t.Fatalf("scoped update changed second: %#v, %v", afterScoped.Files["second.bin"], err)
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
		runOK(t, "pull", "first.bin", "second.bin", "--update-lockfile")
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
		urls := make([]string, 0, assets+1)
		urls = append(urls, "add")
		for index := range assets {
			name := fmt.Sprintf("artifact-%d", index)
			urls = append(urls, fmt.Sprintf("%s/%s.bin", server.URL, name))
		}
		runOK(t, urls...)
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
		runOK(t, "add", server.URL+"/healthy.bin", server.URL+"/broken.bin")
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
		runOK(t, "add", server.URL+"/{{.VERSION}}/artifact.bin")
		runOK(t, "update", "artifact.bin", "--set", "VERSION=one")
		runOK(t, "pull", "--update-lockfile")

		// A stale lock is a configuration failure: the caller edits dac.toml.
		runOK(t, "update", "artifact.bin", "--set", "VERSION=two")
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
		value := manifest.Manifest{Version: manifest.Version, Files: map[string]manifest.Asset{
			name: {URL: server.URL + "/{{.VERSION}}/artifact.bin", File: "artifact.bin"},
		}}
		if err := manifest.Write("dac.toml", value); err != nil {
			t.Fatal(err)
		}
		runOK(t, "update", "--set", "VERSION=one", "--", name)
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
