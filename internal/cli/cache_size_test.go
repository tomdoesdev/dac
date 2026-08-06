package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCacheGCEvictsToASizeBound covers the lever age does not give you. A
// machine whose projects genuinely use more than the disk has to spare has
// nothing old enough to collect and is still too full.
func TestCacheGCEvictsToASizeBound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Distinct bytes per path, each comfortably over the bound below.
		_, _ = io.WriteString(writer, strings.Repeat(request.URL.Path, 200))
	}))
	defer server.Close()

	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	for _, asset := range [][2]string{{"app/one@1", "/a"}, {"app/two@1", "/b"}} {
		assertSuccess(t, runJSON(t, appendArgs(base, "add", asset[0], server.URL+asset[1], "--no-progress")), "add")
	}

	// A bound nothing exceeds takes nothing, however recently used.
	result := runJSON(t, appendArgs(base, "cache", "gc", "--max-age", "30d", "--max-size", "1GiB"))
	assertSuccess(t, result, "cache.gc")
	data := result.value["data"].(map[string]any)
	if data["objectCount"].(float64) != 0 {
		t.Fatalf("a cache inside its bound lost objects: %#v", data)
	}

	// A bound of nothing at all evicts both, though neither is old.
	result = runJSON(t, appendArgs(base, "cache", "gc", "--max-age", "30d", "--max-size", "1"))
	assertSuccess(t, result, "cache.gc")
	data = result.value["data"].(map[string]any)
	if data["objectCount"].(float64) != 2 || data["byteCount"].(float64) <= 0 {
		t.Fatalf("unexpected eviction: %#v", data)
	}
	for _, removed := range []string{"tempCount", "sidecarCount", "evictedCount", "evictedBytes"} {
		if _, found := data[removed]; found {
			t.Fatalf("removed GC field %q is still reported: %#v", removed, data)
		}
	}
}

// TestCacheGCReportsEvictionInTheObjectTotal covers the deliberately compact
// summary shared by age collection and size eviction.
func TestCacheGCReportsEvictionInTheObjectTotal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, strings.Repeat(request.URL.Path, 200))
	}))
	defer server.Close()

	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/one@1", server.URL+"/a", "--no-progress")), "add")

	human := run(t, appendArgs(base, "cache", "gc", "--max-age", "30d", "--max-size", "1"))
	if human.status != ExitOK || !strings.Contains(human.stdout, "Removed 1 object") || strings.Contains(human.stdout, "size bound") {
		t.Fatalf("unexpected eviction summary: %#v", human)
	}
}

// TestCacheGCRejectsAnInvalidSizeBound keeps a typo from being read as no bound.
func TestCacheGCRejectsAnInvalidSizeBound(t *testing.T) {
	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertError(t, runJSON(t, appendArgs(base, "cache", "gc", "--max-size", "banana")), "invalid_arguments")
	assertError(t, runJSON(t, appendArgs(base, "cache", "gc", "--max-size", "0")), "invalid_arguments")
	assertError(t, runJSON(t, appendArgs(base, "cache", "gc", "--max-size", "")), "invalid_arguments")
}
