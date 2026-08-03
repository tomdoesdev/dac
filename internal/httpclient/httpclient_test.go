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

	"github.com/tom/dac/internal/application"
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
