package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/credential"
	"github.com/tom/dac/internal/rewrite"
	"github.com/tom/dac/internal/urlpolicy"
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

	response, err := client.Fetch(context.Background(), application.FetchRequest{URL: server.URL, ETag: "\"old\""})
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
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
	client := New(Options{Timeout: time.Second})
	defer client.Close()

	_, err := client.Fetch(context.Background(), application.FetchRequest{URL: server.URL})
	if !errors.Is(err, urlpolicy.ErrNotPermitted) {
		t.Fatalf("expected URL policy error, got %v", err)
	}
}

func TestFetchAppliesHostPolicyToRedirectTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", "https://blocked.example.com/asset")
		writer.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	config, err := rewrite.Parse(strings.NewReader("block blocked.example.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	client := New(Options{Timeout: time.Second, Rewriter: config})
	defer client.Close()

	_, err = client.Fetch(context.Background(), application.FetchRequest{URL: server.URL})
	if !errors.Is(err, rewrite.ErrBlocked) {
		t.Fatalf("expected ErrBlocked, got %v", err)
	}
}

// credentialHelper writes a helper that answers with one header.
func credentialHelper(t *testing.T, name, value string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helper")
	body := fmt.Sprintf("#!/bin/sh\nprintf '{\"headers\":{%q:[%q]}}'\n", name, value)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestFetchDoesNotForwardCredentialsAcrossARedirect covers the header names a
// registry actually uses. Clearing Authorization alone leaves a helper's
// PRIVATE-TOKEN or X-Api-Key on the request, and net/http copies the initial
// request's headers onto every redirect target, so the second host would
// receive the first host's secret.
func TestFetchDoesNotForwardCredentialsAcrossARedirect(t *testing.T) {
	var received atomic.Value
	received.Store("")
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received.Store(request.Header.Get("X-Api-Key"))
		_, _ = io.WriteString(writer, "asset")
	}))
	defer target.Close()
	// The two servers share an address and differ only in hostname, which is
	// what makes them two credential scopes rather than one.
	elsewhere := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", elsewhere)
		writer.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()

	helper := credentialHelper(t, "X-Api-Key", "origin-secret")
	resolver, err := credential.New([]string{"127.0.0.1=" + helper}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client := New(Options{Timeout: 5 * time.Second, Credentials: resolver})
	defer client.Close()

	response, err := client.Fetch(context.Background(), application.FetchRequest{URL: origin.URL, AllowInsecureHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if value := received.Load().(string); value != "" {
		t.Fatalf("the redirect target received the origin's credential header %q", value)
	}
}

// TestFetchSendsTheCredentialsOfEachHost is the other half: clearing the
// previous host's headers must not stop the new host's helper from supplying
// its own.
func TestFetchSendsTheCredentialsOfEachHost(t *testing.T) {
	var received atomic.Value
	received.Store("")
	target := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received.Store(request.Header.Get("X-Api-Key"))
		_, _ = io.WriteString(writer, "asset")
	}))
	defer target.Close()
	elsewhere := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Location", elsewhere)
		writer.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()

	resolver, err := credential.New([]string{
		"127.0.0.1=" + credentialHelper(t, "X-Api-Key", "origin-secret"),
		"localhost=" + credentialHelper(t, "X-Api-Key", "target-secret"),
	}, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client := New(Options{Timeout: 5 * time.Second, Credentials: resolver})
	defer client.Close()

	response, err := client.Fetch(context.Background(), application.FetchRequest{URL: origin.URL, AllowInsecureHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(response.Body)
	_ = response.Body.Close()
	if value := received.Load().(string); value != "target-secret" {
		t.Fatalf("the redirect target received %q, want its own credentials", value)
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
