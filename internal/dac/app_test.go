package dac

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/project"
)

// TestWorkflow exercises the public trust boundary: only lock accepts current
// remote bytes, while pull restores the bytes that are already locked.
func TestWorkflowLocksAndReproducesBytes(t *testing.T) {
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
		runOK(t, "lock", "artifact")

		// Lock is the explicit acceptance step: it installs the fetched bytes and
		// records their digest together.
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
		stdout, stderr, code := run("pull", "--force")
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
		// Lock must fetch independently rather than reuse preaccepted hidden state
		// from pin calculation.
		runOK(t, "lock", "artifact")
		if requests != 2 {
			t.Fatalf("lock did not download independently; requests=%d", requests)
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
		runOK(t, "lock")
		runOK(t, "update", "artifact", "--set", "VERSION=two")
		_, stderr, code := run("pull", "--json")
		if code != 2 || strings.Contains(stderr, "do-not-print") {
			t.Fatalf("stale JSON error: code=%d stderr=%q", code, stderr)
		}
		var output struct {
			Kind string `json:"kind"`
			Hint string `json:"hint"`
		}
		if err := json.Unmarshal([]byte(stderr), &output); err != nil || output.Kind != "configuration" || output.Hint == "" {
			t.Fatalf("JSON error = %#v, %v", output, err)
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
		runOK(t, "lock")
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
		runOK(t, "update", "namespace/asset@version")

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

// TestLockRejectsDownloadsSymlinkBeforeNetworkAccess checks both filesystem
// confinement and operation ordering. DAC must validate its download root before
// performing an HTTP request so a malicious symlink cannot write outside the
// project or trigger network side effects for an operation that cannot commit.
func TestLockRejectsDownloadsSymlinkBeforeNetworkAccess(t *testing.T) {
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
		_, _, code := run("lock")
		if code != 1 {
			t.Fatalf("lock code=%d, want filesystem failure", code)
		}
		if requests != 0 {
			t.Fatalf("lock made %d HTTP requests before rejecting the symlink", requests)
		}
		entries, err := os.ReadDir(outside)
		if err != nil || len(entries) != 0 {
			t.Fatalf("outside directory changed: %v, %v", entries, err)
		}
	})
}

// TestLockRollsBackDownloadsWhenLockfileCommitFails protects lock's all-or-
// nothing transaction. Downloaded bytes are only accepted together with their
// digest metadata; if dac.lock cannot be installed, the staged artifact must be
// removed rather than left looking trusted.
func TestLockRollsBackDownloadsWhenLockfileCommitFails(t *testing.T) {
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
		_, _, code := run("lock")
		if code != 1 {
			t.Fatalf("lock code=%d, want filesystem failure", code)
		}
		if _, err := os.Stat(filepath.Join(directory, ".dac", "downloads", "artifact.bin")); !os.IsNotExist(err) {
			t.Fatalf("download survived failed lock transaction: %v", err)
		}
	})
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
