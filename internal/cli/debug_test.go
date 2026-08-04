package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDebugTracesToStandardErrorOnly is the constraint tracing runs under.
//
// Standard output carries the output contract, and a command result is the only
// thing allowed onto it. A trace is for a person reading it, so it goes where
// help and progress already go.
func TestDebugTracesToStandardErrorOnly(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "asset bytes")
	}))
	defer server.Close()

	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")

	result := run(t, appendArgs(base, "--json", "--debug", "add", "app/geo@1", server.URL))
	if result.status != ExitOK {
		t.Fatalf("add failed: %#v", result)
	}
	// Still exactly one JSON document, with the trace nowhere near it.
	var envelope map[string]any
	decoder := json.NewDecoder(strings.NewReader(result.stdout))
	if err := decoder.Decode(&envelope); err != nil {
		t.Fatalf("the trace reached stdout: %v\nstdout: %s", err, result.stdout)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		t.Fatalf("stdout carries more than the result: %s", result.stdout)
	}
	if envelope["ok"] != true {
		t.Fatalf("unexpected result: %#v", envelope)
	}
	for _, wanted := range []string{"fetching", "installed", "level=DEBUG"} {
		if !strings.Contains(result.stderr, wanted) {
			t.Fatalf("the trace omitted %q:\n%s", wanted, result.stderr)
		}
	}
}

// TestWithoutDebugNothingIsTraced keeps the default quiet. Tracing is something
// asked for, and a command nobody asked has to say nothing extra at all.
func TestWithoutDebugNothingIsTraced(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "asset bytes")
	}))
	defer server.Close()

	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")

	result := run(t, appendArgs(base, "--json", "add", "app/geo@1", server.URL, "--no-progress"))
	if result.status != ExitOK {
		t.Fatalf("add failed: %#v", result)
	}
	if result.stderr != "" {
		t.Fatalf("a command nobody asked to trace wrote to stderr: %q", result.stderr)
	}
}

// TestDebugTracesTheCacheDecision covers the half of a transfer that is not a
// transfer. Whether the bytes came from the origin or were already on disk is
// the first thing to establish when a pull is faster or slower than expected.
func TestDebugTracesTheCacheDecision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "asset bytes")
	}))
	defer server.Close()

	base := newProject(t).base
	assertSuccess(t, runJSON(t, appendArgs(base, "init")), "init")
	assertSuccess(t, runJSON(t, appendArgs(base, "add", "app/geo@1", server.URL)), "add")

	// Adding resolved it, so the object is already there and the pull is a hit.
	result := run(t, appendArgs(base, "--debug", "pull"))
	if result.status != ExitOK {
		t.Fatalf("pull failed: %#v", result)
	}
	if !strings.Contains(result.stderr, "cache hit") {
		t.Fatalf("the trace did not say the object was cached:\n%s", result.stderr)
	}
	if strings.Contains(result.stderr, "msg=fetching") {
		t.Fatalf("a cached pull traced a request:\n%s", result.stderr)
	}
}

// TestDebugIsReachableFromTheEnvironment covers the spelling a CI job uses,
// where adding a flag to the command means editing whatever generated it.
func TestDebugIsReachableFromTheEnvironment(t *testing.T) {
	t.Setenv("DAC_DEBUG", "1")
	base := newProject(t).base
	result := run(t, appendArgs(base, "init"))
	if result.status != ExitOK {
		t.Fatalf("init failed: %#v", result)
	}
	// init touches neither the network nor the cache, so it has nothing to
	// trace. What matters is that the variable was accepted rather than
	// rejected as an unknown value.
	if strings.Contains(result.stderr, "Error") {
		t.Fatalf("DAC_DEBUG was not accepted: %q", result.stderr)
	}
}
