package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/dac"
)

func TestFetchSendsIdentityAndETagHeaders(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if value := request.Header.Get("Accept-Encoding"); value != "identity" {
			t.Errorf("Accept-Encoding is %q", value)
		}
		if value := request.Header.Get("If-None-Match"); value != "\"old\"" {
			t.Errorf("If-None-Match is %q", value)
		}
		writer.Header().Set("ETag", "\"new\"")
		writer.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()
	client := New(Options{Timeout: time.Second})
	defer client.Close()

	response, err := client.Fetch(context.Background(), dac.FetchRequest{URL: server.URL, ETag: "\"old\""})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if !response.NotModified || response.ETag != "\"new\"" {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestFetchPropagatesLastModifiedConditionally(t *testing.T) {
	const modified = "Wed, 21 Oct 2015 07:28:00 GMT"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if value := request.Header.Get("If-Modified-Since"); value != modified {
			t.Errorf("If-Modified-Since is %q", value)
		}
		writer.Header().Set("Last-Modified", modified)
		writer.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()
	client := New(Options{Timeout: time.Second})
	defer client.Close()

	response, err := client.Fetch(context.Background(), dac.FetchRequest{URL: server.URL, LastModified: modified})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if !response.NotModified || response.LastModified != modified {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestProbeUsesHEADAndReturnsCanonicalValidators(t *testing.T) {
	const modified = "Wed, 21 Oct 2015 07:28:00 GMT"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodHead {
			t.Errorf("method is %s", request.Method)
		}
		writer.Header().Set("ETag", "W/\"asset\"")
		writer.Header().Set("Last-Modified", modified)
		writer.Header().Set("Content-Length", "12")
	}))
	defer server.Close()
	client := New(Options{Timeout: time.Second})
	defer client.Close()

	response, err := client.Probe(context.Background(), dac.ProbeRequest{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if response.ETag != "W/\"asset\"" || response.LastModified != modified || response.Length != 12 {
		t.Fatalf("unexpected probe response: %#v", response)
	}
}

func TestProbeAppliesRedirectsAndRetries(t *testing.T) {
	var responses atomic.Int32
	methods := map[string]bool{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		methods[request.Method] = true
		if request.URL.Path == "/source" {
			http.Redirect(writer, request, server.URL+"/asset", http.StatusFound)
			return
		}
		if responses.Add(1) == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.Header().Set("ETag", "\"settled\"")
	}))
	defer server.Close()
	client := New(Options{
		Timeout: time.Second,
		Retries: 1,
	})
	defer client.Close()

	response, err := client.Probe(context.Background(), dac.ProbeRequest{
		URL: server.URL + "/source",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.ETag != "\"settled\"" || responses.Load() != 2 {
		t.Fatalf("unexpected retried probe: %#v responses=%d", response, responses.Load())
	}
	if methods[http.MethodGet] {
		t.Fatal("a HEAD redirect or retry became GET")
	}
}

func TestFetchRetriesTransientStatus(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(writer, "asset")
	}))
	defer server.Close()
	client := New(Options{Timeout: time.Second, Retries: 1})
	defer client.Close()

	response, err := client.Fetch(context.Background(), dac.FetchRequest{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "asset" || requests.Load() != 2 {
		t.Fatalf("content=%q requests=%d", content, requests.Load())
	}
}

func TestFetchStopsAStalledBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", "10")
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
	}))
	defer server.Close()
	client := New(Options{Timeout: 20 * time.Millisecond})
	defer client.Close()

	response, err := client.Fetch(context.Background(), dac.FetchRequest{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !errors.Is(err, dac.ErrStalled) {
		t.Fatalf("expected ErrStalled, got %v", err)
	}
}

func TestFetchRejectsEncodedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Encoding", "gzip")
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := New(Options{Timeout: time.Second})
	defer client.Close()

	_, err := client.Fetch(context.Background(), dac.FetchRequest{URL: server.URL})
	if err == nil || !strings.Contains(err.Error(), "not identity") {
		t.Fatalf("expected identity encoding error, got %v", err)
	}
}

// The header is the origin naming the asset outright, so it beats the URL even
// when the URL spells something perfectly usable. An origin that sends both has
// told us the path element is not the name it wants used.
func TestFetchPrefersTheNameTheHeaderGives(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Disposition", `attachment; filename="database.bin"`)
		_, _ = io.WriteString(writer, "asset")
	}))
	defer server.Close()
	client := New(Options{Timeout: time.Second})
	defer client.Close()

	response, err := client.Fetch(context.Background(), dac.FetchRequest{URL: server.URL + "/download?id=1234"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.Filename != "database.bin" {
		t.Fatalf("filename is %q", response.Filename)
	}
}

func TestFetchFallsBackToTheNameTheURLSpells(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "asset")
	}))
	defer server.Close()
	client := New(Options{Timeout: time.Second})
	defer client.Close()

	response, err := client.Fetch(context.Background(), dac.FetchRequest{URL: server.URL + "/geo/database.bin?token=abc"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.Filename != "database.bin" {
		t.Fatalf("filename is %q", response.Filename)
	}
}

// The URL a request finishes at is the one that names the asset. A download
// endpoint that redirects to a distribution host is the ordinary shape of this,
// and the opaque URL the manifest holds names nothing.
func TestFetchNamesTheAssetFromTheRedirectTarget(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/download" {
			http.Redirect(writer, request, server.URL+"/files/database.bin", http.StatusFound)
			return
		}
		_, _ = io.WriteString(writer, "asset")
	}))
	defer server.Close()
	client := New(Options{Timeout: time.Second})
	defer client.Close()

	response, err := client.Fetch(context.Background(), dac.FetchRequest{URL: server.URL + "/download"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.Filename != "database.bin" {
		t.Fatalf("filename is %q", response.Filename)
	}
}

// Not every origin names anything, and an empty name is a valid answer rather
// than a failure: the field decides nothing downstream.
func TestFetchReportsNoNameWhenNothingSpellsOne(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "asset")
	}))
	defer server.Close()
	client := New(Options{Timeout: time.Second})
	defer client.Close()

	response, err := client.Fetch(context.Background(), dac.FetchRequest{URL: server.URL + "/"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.Filename != "" {
		t.Fatalf("filename is %q, want none", response.Filename)
	}
}

// A header naming a path is refused rather than repaired, and the URL answers
// instead. Nothing about the request fails over it.
func TestFetchRefusesAHeaderNameThatEscapesItsDirectory(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Disposition", `attachment; filename="../../etc/passwd"`)
		_, _ = io.WriteString(writer, "asset")
	}))
	defer server.Close()
	client := New(Options{Timeout: time.Second})
	defer client.Close()

	response, err := client.Fetch(context.Background(), dac.FetchRequest{URL: server.URL + "/geo/database.bin"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.Filename != "database.bin" {
		t.Fatalf("filename is %q, want the URL name", response.Filename)
	}
}
