package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
)

// The assets every test here builds a project from, and the path each is served
// at, so an assertion can name the requests a narrowed pull should have made.
var pullAssets = []struct{ coordinate, path string }{
	{"app/geo@1.0.0", "/geo-1.bin"},
	{"app/geo@2.0.0", "/geo-2.bin"},
	{"tools/kit@3.0.0", "/kit.bin"},
}

// TestPullFetchesOnlyTheAssetsNamed covers the reason a pull can be narrowed.
//
// A project holds every asset every job built from it needs. Without a filter,
// a job that needs one of them fetches all of them: dac path requires the
// object to be cached already, so a plain pull was the only way to put it
// there.
func TestPullFetchesOnlyTheAssetsNamed(t *testing.T) {
	paths, requested := newPullProject(t)

	result := runJSON(t, appendArgs(paths.base, "pull", "app/geo@1.0.0"))
	assertSuccess(t, result, "pull")
	data := result.value["data"].(map[string]any)
	if data["assetCount"] != float64(1) || data["projectCount"] != float64(3) {
		t.Fatalf("unexpected counts: %#v", data)
	}
	if made := requested.paths(); !slices.Equal(made, []string{"/geo-1.bin"}) {
		t.Fatalf("pull requested %v, want only /geo-1.bin", made)
	}

	// The rest of the project is still missing, which is the whole point.
	listing := runJSON(t, appendArgs(paths.base, "cache", "list"))
	assertSuccess(t, listing, "cache.list")
	cached := listing.value["data"].(map[string]any)
	if cached["objectCount"] != float64(1) || cached["missingCount"] != float64(2) {
		t.Fatalf("unexpected cache listing: %#v", cached)
	}
}

// TestPullNarrowedByAssetTakesEveryVersion covers the shorter spelling. A bare
// namespace/name is how info asks about an asset, and narrowing a pull is the
// same question with a different verb.
func TestPullNarrowedByAssetTakesEveryVersion(t *testing.T) {
	paths, requested := newPullProject(t)

	result := runJSON(t, appendArgs(paths.base, "pull", "app/geo"))
	assertSuccess(t, result, "pull")
	data := result.value["data"].(map[string]any)
	if data["assetCount"] != float64(2) || data["projectCount"] != float64(3) {
		t.Fatalf("unexpected counts: %#v", data)
	}
	made := requested.paths()
	slices.Sort(made)
	if !slices.Equal(made, []string{"/geo-1.bin", "/geo-2.bin"}) {
		t.Fatalf("pull requested %v, want both versions of app/geo", made)
	}
}

// TestPullFetchesOverlappingSelectionsOnce covers a coordinate named twice,
// once whole and once by its asset. A selection is a filter over the project
// rather than a list of fetches to run.
func TestPullFetchesOverlappingSelectionsOnce(t *testing.T) {
	paths, requested := newPullProject(t)

	result := runJSON(t, appendArgs(paths.base, "pull", "app/geo", "app/geo@1.0.0"))
	assertSuccess(t, result, "pull")
	data := result.value["data"].(map[string]any)
	if data["assetCount"] != float64(2) {
		t.Fatalf("unexpected asset count: %#v", data)
	}
	if count := len(requested.paths()); count != 2 {
		t.Fatalf("pull made %d requests, want one per asset", count)
	}
}

// TestPullWithoutArgumentsStillTakesTheProject is the case that must not have
// changed. Every existing script runs this one.
func TestPullWithoutArgumentsStillTakesTheProject(t *testing.T) {
	paths, requested := newPullProject(t)

	human := run(t, appendArgs(paths.base, "pull", "--no-progress"))
	if human.status != ExitOK || human.stdout != "Pulled 3 assets.\n" {
		t.Fatalf("unexpected pull summary: %#v", human)
	}
	if count := len(requested.paths()); count != 3 {
		t.Fatalf("pull made %d requests, want the whole project", count)
	}
}

// TestPullSaysWhenItTookOnlyPartOfTheProject covers the summary. "Pulled 1
// asset" is true of a project with one asset and of a job that asked for one of
// twenty, and the difference is why assets can be named at all.
func TestPullSaysWhenItTookOnlyPartOfTheProject(t *testing.T) {
	paths, _ := newPullProject(t)

	human := run(t, appendArgs(paths.base, "pull", "app/geo@1.0.0", "--no-progress"))
	if human.status != ExitOK || human.stdout != "Pulled 1 asset of 3.\n" {
		t.Fatalf("unexpected narrowed pull summary: %#v", human)
	}
}

// TestPullRefusesAnAssetTheProjectDoesNotHave keeps a typo from being read as
// "fetch nothing and report success".
func TestPullRefusesAnAssetTheProjectDoesNotHave(t *testing.T) {
	paths, _ := newPullProject(t)

	assertError(t, runJSON(t, appendArgs(paths.base, "pull", "app/geo@9.9.9")), "asset_unknown")
	assertError(t, runJSON(t, appendArgs(paths.base, "pull", "nope/missing")), "asset_unknown")
	assertError(t, runJSON(t, appendArgs(paths.base, "pull", "not-a-coordinate")), "invalid_arguments")
}

// TestPullNarrowedStillReconcilesTheWholeLock checks that selecting installs
// does not permit pull to write an incomplete lock.
func TestPullNarrowedStillReconcilesTheWholeLock(t *testing.T) {
	paths, _ := newPullProject(t)
	// A manifest edit that nothing has locked, which is what a hand edit leaves.
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", "extra/thing@1", "https://example.com/x.bin", "--offline")), "add")

	assertError(t, runJSON(t, appendArgs(paths.base, "pull", "app/geo@1.0.0")), "host_not_trusted")
}

// TestNarrowedRefreshReachesOnlyTheNamedOrigins covers what naming assets buys a
// refresh. The lock file it writes still has to describe the whole project, so the
// narrowing is over the origins it asks rather than over the file it leaves behind.
func TestNarrowedRefreshReachesOnlyTheNamedOrigins(t *testing.T) {
	paths, requested := newPullProject(t)

	result := runJSON(t, appendArgs(paths.base, "pull", "app/geo@1.0.0", "--refresh", "--no-progress"))
	assertSuccess(t, result, "pull")
	data := result.value["data"].(map[string]any)
	if locked := data["locked"].([]any); len(locked) != 1 || locked[0] != "app/geo@1.0.0" {
		t.Fatalf("the refresh resolved more than the asset it named: %#v", locked)
	}
	if made := requested.paths(); !slices.Equal(made, []string{"/geo-1.bin"}) {
		t.Fatalf("refresh requested %v, want only /geo-1.bin", made)
	}
	// The two files still agree, which is the only kind of lock file a later run accepts.
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "check")), "check")
}

// newPullProject builds a locked three-asset project with an empty cache, and
// returns the log of what the origin is asked for from that point on.
//
// The cache is emptied after the setup pull resolves the assets. A pull with
// nothing left to fetch would make every assertion here
// pass without a request being skipped or made.
func newPullProject(t *testing.T) (projectFlags, *requestLog) {
	t.Helper()
	log := &requestLog{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		log.record(request.URL.Path)
		_, _ = io.WriteString(writer, "bytes for "+request.URL.Path)
	}))
	t.Cleanup(server.Close)

	paths := newProject(t)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "init")), "init")
	for _, asset := range pullAssets {
		assertSuccess(t, runJSON(t, appendArgs(paths.base, "add", asset.coordinate, server.URL+asset.path)), "add")
	}
	settleCLIProject(t, paths.base)
	assertSuccess(t, runJSON(t, appendArgs(paths.base, "cache", "clear")), "cache.clear")
	log.reset()
	return paths, log
}

// requestLog records the paths an origin was asked for. A narrowed pull is only
// worth anything if the requests it does not make are the ones it skipped, so
// counting them is the assertion rather than a detail of it.
type requestLog struct {
	mutex sync.Mutex
	seen  []string
}

func (log *requestLog) record(path string) {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	log.seen = append(log.seen, path)
}

func (log *requestLog) paths() []string {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	return append([]string{}, log.seen...)
}

func (log *requestLog) reset() {
	log.mutex.Lock()
	defer log.mutex.Unlock()
	log.seen = nil
}
