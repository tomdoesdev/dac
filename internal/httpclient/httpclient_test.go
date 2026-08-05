package httpclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/urlpolicy"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

// RoundTrip lets a test use a function as an HTTP transport.
func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

// countRequests returns a decorator that counts each HTTP request.
func countRequests(count *atomic.Int32) TransportDecorator {
	return func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(request *http.Request) (*http.Response, error) {
			count.Add(1)
			return next.RoundTrip(request)
		})
	}
}

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

	response, err := client.Fetch(context.Background(), application.FetchRequest{URL: server.URL, ETag: "\"old\""})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if !response.NotModified || response.ETag != "\"new\"" {
		t.Fatalf("unexpected response: %#v", response)
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

	response, err := client.Fetch(context.Background(), application.FetchRequest{URL: server.URL})
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

func TestTransportDecoratorSeesRedirectsAndRetries(t *testing.T) {
	var responses atomic.Int32
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/source" {
			http.Redirect(writer, request, server.URL+"/asset", http.StatusFound)
			return
		}
		if responses.Add(1) == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(writer, "asset")
	}))
	defer server.Close()

	var requests atomic.Int32
	client := New(Options{
		Timeout:             time.Second,
		Retries:             1,
		TransportDecorators: []TransportDecorator{countRequests(&requests)},
	})
	defer client.Close()
	response, err := client.Fetch(context.Background(), application.FetchRequest{
		URL:               server.URL + "/source",
		AllowInsecureHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if requests.Load() != 4 {
		t.Fatalf("decorated requests = %d, want 4", requests.Load())
	}
}

// TestARefusedRequestIsNotRetried covers what a transport decorator needs in
// order to refuse anything at all. Retrying defaults to yes, so without the
// permanent list a refusal would be re-asked once per retry and once per range
// of a split download, and every one of them would be refused again.
func TestARefusedRequestIsNotRetried(t *testing.T) {
	var requests atomic.Int32
	refuse := func(http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(request *http.Request) (*http.Response, error) {
			requests.Add(1)
			return nil, &application.HostError{Host: request.URL.Hostname()}
		})
	}
	client := New(Options{
		Timeout:             time.Second,
		Retries:             3,
		TransportDecorators: []TransportDecorator{refuse},
	})
	defer client.Close()

	_, err := client.Fetch(context.Background(), application.FetchRequest{URL: "https://example.com/asset"})
	if !errors.Is(err, application.ErrHostNotTrusted) {
		t.Fatalf("error is %v, want a refusal", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", requests.Load())
	}
}

// TestADecoratorSeesTheHostARedirectMovesTo is why the trust check belongs in
// the transport: the URL DAC was given is not the only host it would talk to.
func TestADecoratorSeesTheHostARedirectMovesTo(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "asset")
	}))
	defer origin.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, origin.URL+"/asset", http.StatusFound)
	}))
	defer redirect.Close()

	// The first host is allowed and the one it redirects to is not, so only a
	// decorator that sees the second hop can tell the difference.
	allowed := mustHostPort(t, redirect.URL)
	refuseOthers := func(next http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Host != allowed {
				return nil, &application.HostError{Host: request.URL.Hostname()}
			}
			return next.RoundTrip(request)
		})
	}
	client := New(Options{
		Timeout:             time.Second,
		Retries:             2,
		TransportDecorators: []TransportDecorator{refuseOthers},
	})
	defer client.Close()

	_, err := client.Fetch(context.Background(), application.FetchRequest{
		URL:               redirect.URL + "/source",
		AllowInsecureHTTP: true,
	})
	if !errors.Is(err, application.ErrHostNotTrusted) {
		t.Fatalf("error is %v, want a refusal", err)
	}
	var refusal *application.HostError
	if !errors.As(err, &refusal) || refusal.Host != mustHostname(t, origin.URL) {
		t.Fatalf("error names %v, want the host the redirect moved to", err)
	}
}

func mustHostPort(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("Parse(%q): %v", rawURL, err)
	}
	return parsed.Host
}

func mustHostname(t *testing.T, rawURL string) string {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("Parse(%q): %v", rawURL, err)
	}
	return parsed.Hostname()
}

func TestURLPolicyRunsBeforeTransportDecorators(t *testing.T) {
	var requests atomic.Int32
	client := New(Options{
		Timeout:             time.Second,
		TransportDecorators: []TransportDecorator{countRequests(&requests)},
	})
	defer client.Close()

	_, err := client.Fetch(context.Background(), application.FetchRequest{URL: "http://example.com/asset"})
	if !errors.Is(err, urlpolicy.ErrNotPermitted) {
		t.Fatalf("expected URL policy error, got %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("decorator saw %d blocked requests", requests.Load())
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

	response, err := client.Fetch(context.Background(), application.FetchRequest{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if !errors.Is(err, application.ErrStalled) {
		t.Fatalf("expected ErrStalled, got %v", err)
	}
}

func TestFetchRejectsUnsafeRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", "http://example.com/asset")
		writer.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	var requests atomic.Int32
	client := New(Options{
		Timeout:             time.Second,
		TransportDecorators: []TransportDecorator{countRequests(&requests)},
	})
	defer client.Close()

	_, err := client.Fetch(context.Background(), application.FetchRequest{URL: server.URL})
	if !errors.Is(err, urlpolicy.ErrNotPermitted) {
		t.Fatalf("expected URL policy error, got %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("decorator saw blocked redirect request: count=%d", requests.Load())
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

	_, err := client.Fetch(context.Background(), application.FetchRequest{URL: server.URL})
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

	response, err := client.Fetch(context.Background(), application.FetchRequest{URL: server.URL + "/download?id=1234"})
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

	response, err := client.Fetch(context.Background(), application.FetchRequest{URL: server.URL + "/geo/database.bin?token=abc"})
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

	response, err := client.Fetch(context.Background(), application.FetchRequest{URL: server.URL + "/download"})
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

	response, err := client.Fetch(context.Background(), application.FetchRequest{URL: server.URL + "/"})
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

	response, err := client.Fetch(context.Background(), application.FetchRequest{URL: server.URL + "/geo/database.bin"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.Filename != "database.bin" {
		t.Fatalf("filename is %q, want the URL name", response.Filename)
	}
}
