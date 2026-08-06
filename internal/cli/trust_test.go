package cli

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/trust"
)

// trustFlags points one test at its own trusted-hosts file.
// The package-wide file TestMain seeds trusts the loopback names, which is what
// every other test needs and the opposite of what these tests are about.
func trustFlags(t *testing.T) (string, []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trusted-hosts.json")
	return path, []string{"--trust-file", path}
}

func seedTrust(t *testing.T, path string, hosts ...string) {
	t.Helper()
	if _, err := trust.New(path).Update(context.Background(), func(list trust.List) (trust.List, error) {
		updated, _ := list.Add(hosts, time.Now().UTC())
		return updated, nil
	}); err != nil {
		t.Fatalf("seed trusted hosts: %v", err)
	}
}

func readTrust(t *testing.T, path string) trust.List {
	t.Helper()
	list, err := trust.New(path).Load()
	if err != nil {
		t.Fatalf("read trusted hosts: %v", err)
	}
	return list
}

// assetServer serves one asset and reports how many requests reached it.
func assetServer(t *testing.T, content string) (*httptest.Server, func() int) {
	t.Helper()
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = io.WriteString(writer, content)
	}))
	t.Cleanup(server.Close)
	return server, func() int { return requests }
}

func TestTrustAddListAndRemoveRoundTrip(t *testing.T) {
	path, flags := trustFlags(t)

	added := runJSON(t, appendArgs(flags, "trust", "add", "beta.example", "ALPHA.example:443"))
	assertSuccess(t, added, "trust.add")
	data := added.value["data"].(map[string]any)
	if names := data["added"].([]any); len(names) != 2 || names[0] != "alpha.example" || names[1] != "beta.example" {
		t.Fatalf("added = %#v, want both hosts normalized and in order", data["added"])
	}
	if data["path"] != path {
		t.Fatalf("path = %v, want %q", data["path"], path)
	}

	// Adding a host that is already trusted reports it rather than failing.
	again := runJSON(t, appendArgs(flags, "trust", "add", "alpha.example"))
	assertSuccess(t, again, "trust.add")
	againData := again.value["data"].(map[string]any)
	if len(againData["added"].([]any)) != 0 || againData["present"].([]any)[0] != "alpha.example" {
		t.Fatalf("re-add reported %#v, want no addition and one already present", againData)
	}

	human := run(t, appendArgs(flags, "trust", "list"))
	expected := "alpha.example  never used\n" +
		"beta.example   never used\n" +
		"2 trusted hosts.\n"
	if human.status != ExitOK || human.stdout != expected || human.stderr != "" {
		t.Fatalf("unexpected human trust list: %#v", human)
	}

	removed := runJSON(t, appendArgs(flags, "trust", "remove", "beta.example", "absent.example"))
	assertSuccess(t, removed, "trust.remove")
	removedData := removed.value["data"].(map[string]any)
	if removedData["removed"].([]any)[0] != "beta.example" || removedData["absent"].([]any)[0] != "absent.example" {
		t.Fatalf("remove reported %#v, want one removal and one absence", removedData)
	}
	if list := readTrust(t, path); list.Trusted("beta.example") || !list.Trusted("alpha.example") {
		t.Fatalf("hosts = %v, want only alpha.example", list.Hosts)
	}
}

// TestAnEmptyTrustListSaysWhatToDoAboutIt matters because it is the first thing
// somebody sees, and a bare "none" would not say how to change that.
func TestAnEmptyTrustListSaysWhatToDoAboutIt(t *testing.T) {
	_, flags := trustFlags(t)
	human := run(t, appendArgs(flags, "trust", "list"))
	if human.status != ExitOK || human.stdout != "No trusted hosts. Run dac trust add <host>.\n" {
		t.Fatalf("unexpected empty trust list: %#v", human)
	}
	result := runJSON(t, appendArgs(flags, "trust", "list"))
	assertSuccess(t, result, "trust.list")
	if hosts := result.value["data"].(map[string]any)["hosts"]; hosts == nil {
		t.Fatalf("hosts = %#v, want an empty array rather than null", hosts)
	}
}

// TestADownloadFromAnUntrustedHostIsRefused is the whole feature.
// Nothing may reach the origin, and the message has to name the one command that
// changes the answer, in both output modes.
func TestADownloadFromAnUntrustedHostIsRefused(t *testing.T) {
	server, requests := assetServer(t, "asset bytes")
	paths := newProject(t)
	_, trusted := trustFlags(t)
	base := appendArgs(paths.base, trusted...)
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")

	result := runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL))
	assertError(t, result, "host_not_trusted")
	errorValue := result.value["error"].(map[string]any)
	if !strings.Contains(errorValue["message"].(string), "dac trust add 127.0.0.1") {
		t.Fatalf("message = %q, want the command that fixes it", errorValue["message"])
	}
	details := errorValue["details"].(map[string]any)
	if details["host"] != "127.0.0.1" || details["url"] != server.URL {
		t.Fatalf("details = %#v, want the refused host and the asset URL", details)
	}
	if requests() != 0 {
		t.Fatalf("the origin saw %d requests, want 0", requests())
	}

	human := run(t, appendArgs(base, "add", "app/geo@1", server.URL))
	if human.status != ExitFailure || !strings.Contains(human.stderr, "Error: DAC will not download from 127.0.0.1.") {
		t.Fatalf("unexpected human refusal: %#v", human)
	}
	if human.stdout != "" {
		t.Fatalf("stdout = %q, want nothing on a failure", human.stdout)
	}
}

func TestAddWithTrustRecordsTheHostAndDownloads(t *testing.T) {
	server, requests := assetServer(t, "asset bytes")
	paths := newProject(t)
	path, trusted := trustFlags(t)
	base := appendArgs(paths.base, trusted...)
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")

	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--trust")), "add")
	if requests() != 1 {
		t.Fatalf("the origin saw %d requests, want 1", requests())
	}
	list := readTrust(t, path)
	if !list.Trusted("127.0.0.1") {
		t.Fatalf("hosts = %v, want the added host trusted", list.Hosts)
	}
	// The same run that trusted the host also downloaded from it.
	if list.Hosts["127.0.0.1"].LastUsed.IsZero() {
		t.Fatal("the host has no use time after the download that trusted it")
	}
}

// TestTrustAllDownloadsWithoutTrustingAnything keeps the escape hatch from
// quietly becoming a way to grow the trust list.
func TestTrustAllDownloadsWithoutTrustingAnything(t *testing.T) {
	server, requests := assetServer(t, "asset bytes")
	paths := newProject(t)
	path, trusted := trustFlags(t)
	base := appendArgs(paths.base, trusted...)
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")

	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--insecure-trust-all")), "add")
	if requests() != 1 {
		t.Fatalf("the origin saw %d requests, want 1", requests())
	}
	if list := readTrust(t, path); len(list.Hosts) != 0 {
		t.Fatalf("hosts = %v, want none", list.Hosts)
	}
}

// TestARedirectToAnUntrustedHostIsRefused is why the check is a transport stage.
// The URL in the manifest is not the only host the download would reach, and the
// error has to name the host it was actually refused at.
func TestARedirectToAnUntrustedHostIsRefused(t *testing.T) {
	origin, requests := assetServer(t, "asset bytes")
	gateway := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, origin.URL+"/asset", http.StatusFound)
	}))
	defer gateway.Close()
	// The two servers differ only by the name they are reached under, because the
	// matching rule ignores the port that would otherwise tell them apart.
	source := "http://localhost:" + mustPort(t, gateway.URL) + "/source"

	paths := newProject(t)
	path, trusted := trustFlags(t)
	seedTrust(t, path, "localhost")
	base := appendArgs(paths.base, trusted...)
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")

	result := runJSON(t, appendArgs(base, "add", "app/geo@1", source))
	assertError(t, result, "host_not_trusted")
	details := result.value["error"].(map[string]any)["details"].(map[string]any)
	if details["host"] != "127.0.0.1" {
		t.Fatalf("details = %#v, want the host the redirect moved to", details)
	}
	if details["url"] != source {
		t.Fatalf("details = %#v, want the asset URL that was asked for", details)
	}
	if requests() != 0 {
		t.Fatalf("the origin saw %d requests, want 0", requests())
	}
}

// TestADownloadRecordsWhenItsHostWasLastUsed is what makes collection possible.
func TestADownloadRecordsWhenItsHostWasLastUsed(t *testing.T) {
	server, _ := assetServer(t, "asset bytes")
	paths := newProject(t)
	path, trusted := trustFlags(t)
	base := appendArgs(paths.base, trusted...)
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")

	// An offline add trusts the host without reaching it, so nothing has used it yet.
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--trust", "--offline")), "add")
	if !readTrust(t, path).Hosts["127.0.0.1"].LastUsed.IsZero() {
		t.Fatal("a run that made no request recorded a use")
	}

	before := time.Now().UTC()
	assertSuccess(t, runJSON(t, appendArgs(base, "pull", "--refresh")), "pull")
	used := readTrust(t, path).Hosts["127.0.0.1"].LastUsed
	if used.Before(before) {
		t.Fatalf("lastUsed = %v, want a time from the run that downloaded", used)
	}
}

func TestTrustGCWithdrawsTrustFromHostsNothingHasUsed(t *testing.T) {
	path, flags := trustFlags(t)
	seedTrust(t, path, "alpha.example", "beta.example")

	// Nothing has been used, so the age each host is judged by is when it was added.
	keeps := runJSON(t, appendArgs(flags, "trust", "gc", "--max-age", "365d"))
	assertSuccess(t, keeps, "trust.gc")
	if keeps.value["data"].(map[string]any)["hostCount"] != float64(0) {
		t.Fatalf("gc = %#v, want nothing collected", keeps.value["data"])
	}

	dry := runJSON(t, appendArgs(flags, "trust", "gc", "--max-age", "0d", "--dry-run"))
	assertSuccess(t, dry, "trust.gc")
	dryData := dry.value["data"].(map[string]any)
	if dryData["hostCount"] != float64(2) || dryData["dryRun"] != true {
		t.Fatalf("gc = %#v, want both hosts reported", dryData)
	}
	if len(readTrust(t, path).Hosts) != 2 {
		t.Fatal("a dry run changed the trusted-hosts file")
	}

	collected := runJSON(t, appendArgs(flags, "trust", "gc", "--max-age", "0d"))
	assertSuccess(t, collected, "trust.gc")
	if len(readTrust(t, path).Hosts) != 0 {
		t.Fatalf("hosts = %v, want none", readTrust(t, path).Hosts)
	}
}

// TestTrustGCReadsItsAgeFromTheConfig covers the flag-over-config precedence the
// other bounded commands follow.
func TestTrustGCReadsItsAgeFromTheConfig(t *testing.T) {
	path, flags := trustFlags(t)
	seedTrust(t, path, "alpha.example")
	base := appendArgs(flags, configFlags(t, "schema-version = 2\n\n[trust]\nmax-age = \"0d\"\n")...)

	fromConfig := runJSON(t, appendArgs(base, "trust", "gc", "--dry-run"))
	assertSuccess(t, fromConfig, "trust.gc")
	if fromConfig.value["data"].(map[string]any)["hostCount"] != float64(1) {
		t.Fatalf("gc = %#v, want the config age to collect the host", fromConfig.value["data"])
	}

	fromFlag := runJSON(t, appendArgs(base, "trust", "gc", "--max-age", "365d", "--dry-run"))
	assertSuccess(t, fromFlag, "trust.gc")
	if fromFlag.value["data"].(map[string]any)["hostCount"] != float64(0) {
		t.Fatalf("gc = %#v, want the flag to beat the config", fromFlag.value["data"])
	}
}

func TestInfoReportsWhetherEachAssetHostIsTrusted(t *testing.T) {
	server, _ := assetServer(t, "asset bytes")
	paths := newProject(t)
	path, trusted := trustFlags(t)
	base := appendArgs(paths.base, trusted...)
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL, "--trust")), "add")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/far@1", "https://untrusted.example/asset", "--offline")), "add")

	result := runJSON(t, appendArgs(base, "info"))
	assertSuccess(t, result, "info")
	data := result.value["data"].(map[string]any)
	if data["summary"].(map[string]any)["untrustedCount"] != float64(1) {
		t.Fatalf("summary = %#v, want one untrusted asset", data["summary"])
	}
	states := map[string]string{}
	for _, value := range data["assets"].([]any) {
		asset := value.(map[string]any)
		states[asset["coordinate"].(string)] = asset["trustStatus"].(string)
		if asset["host"] == "" {
			t.Fatalf("asset = %#v, want the host its source URL names", asset)
		}
	}
	if states["app/geo@1"] != "trusted" || states["app/far@1"] != "untrusted" {
		t.Fatalf("trust states = %#v, want one of each", states)
	}

	// Withdrawing trust changes what info says without touching the project.
	assertSuccess(t, runJSON(t, appendArgs(base, "trust", "remove", "127.0.0.1")), "trust.remove")
	if len(readTrust(t, path).Hosts) != 0 {
		t.Fatal("the host is still trusted")
	}
	after := runJSON(t, appendArgs(base, "info"))
	assertSuccess(t, after, "info")
	if after.value["data"].(map[string]any)["summary"].(map[string]any)["untrustedCount"] != float64(2) {
		t.Fatalf("summary = %#v, want both assets untrusted", after.value["data"])
	}
}

func TestTrustPathReportsTheResolvedFile(t *testing.T) {
	path, flags := trustFlags(t)
	human := run(t, appendArgs(flags, "trust", "path"))
	if human.status != ExitOK || human.stdout != path+"\n" || human.stderr != "" {
		t.Fatalf("unexpected trust path: %#v", human)
	}
	result := runJSON(t, appendArgs(flags, "trust", "path"))
	assertSuccess(t, result, "trust.path")
	if result.value["data"].(map[string]any)["path"] != path {
		t.Fatalf("path = %#v, want %q", result.value["data"], path)
	}
}

// TestATrustedHostsFileDACCannotReadFailsTheCommand is deliberate: a refusal
// nobody can evaluate is not a refusal, so DAC says so rather than carrying on.
func TestATrustedHostsFileDACCannotReadFailsTheCommand(t *testing.T) {
	path, flags := trustFlags(t)
	if err := os.WriteFile(path, []byte("trust everything\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertError(t, runJSON(t, appendArgs(flags, "trust", "list")), "trust_file_invalid")

	paths := newProject(t)
	base := appendArgs(paths.base, flags...)
	assertSuccess(t, runJSONArgs(t, append([]string{"--json"}, appendArgs(paths.base, "init")...)), "init")
	assertError(t, runJSON(t, appendArgs(base, "info")), "trust_file_invalid")
}

func TestTrustCommandsRejectArgumentsTheyCannotUse(t *testing.T) {
	_, flags := trustFlags(t)
	cases := []struct {
		name string
		args []string
	}{
		{name: "add with no host", args: []string{"trust", "add"}},
		{name: "add with a path for a host", args: []string{"trust", "add", "example.com/asset"}},
		{name: "add with an empty host", args: []string{"trust", "add", "   "}},
		{name: "remove with no host", args: []string{"trust", "remove"}},
		{name: "list with an argument", args: []string{"trust", "list", "example.com"}},
		{name: "gc with an argument", args: []string{"trust", "gc", "example.com"}},
		{name: "gc with an invalid age", args: []string{"trust", "gc", "--max-age", "soon"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			assertError(t, runJSON(t, appendArgs(flags, test.args...)), "invalid_arguments")
		})
	}
	// A bare group prints help, and an unknown subcommand is a usage error.
	if result := run(t, appendArgs(flags, "trust")); result.status != ExitOK {
		t.Fatalf("bare trust = %#v, want help and success", result)
	}
	unknown := runJSON(t, appendArgs(flags, "trust", "bogus"))
	assertError(t, unknown, "invalid_arguments")
	if unknown.status != ExitUsage {
		t.Fatalf("status = %d, want %d", unknown.status, ExitUsage)
	}
}

func mustPort(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("Parse(%q): %v", rawURL, err)
	}
	return parsed.Port()
}
