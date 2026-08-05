package httpclient

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/application"
)

// assetSize is large enough to split: the first chunk arrives on the response
// that started the download and two more are requested as ranges.
const assetSize = 2*chunkSize + 4<<20

// rangeServer serves one asset, optionally as byte ranges, and records what it
// was asked for.
type rangeServer struct {
	data         []byte
	etag         string
	lastModified string
	acceptRanges bool
	// reject returns the status one ranged request answers with, by the order
	// the server received it. A zero status serves the range.
	reject func(count int64) int

	requests atomic.Int64
	parts    atomic.Int64
	matched  atomic.Int64
}

func newRangeServer(size int) *rangeServer {
	data := make([]byte, size)
	for index := range data {
		data[index] = byte(index*31 + index/1021)
	}
	return &rangeServer{data: data, etag: "\"v1\"", acceptRanges: true}
}

func (server *rangeServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.requests.Add(1)
	if server.acceptRanges {
		writer.Header().Set("Accept-Ranges", "bytes")
	}
	if server.etag != "" {
		writer.Header().Set("ETag", server.etag)
	}
	if server.lastModified != "" {
		writer.Header().Set("Last-Modified", server.lastModified)
	}
	spec := request.Header.Get("Range")
	if spec == "" {
		writer.Header().Set("Content-Length", strconv.Itoa(len(server.data)))
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(server.data)
		return
	}
	count := server.parts.Add(1)
	if request.Header.Get("If-Match") == server.etag && server.etag != "" {
		server.matched.Add(1)
	}
	if server.reject != nil {
		if status := server.reject(count); status != 0 {
			writer.WriteHeader(status)
			return
		}
	}
	first, last, err := parseRangeSpec(spec, int64(len(server.data)))
	if err != nil {
		writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(server.data)))
	writer.Header().Set("Content-Length", strconv.FormatInt(last-first+1, 10))
	writer.WriteHeader(http.StatusPartialContent)
	_, _ = writer.Write(server.data[first : last+1])
}

func parseRangeSpec(spec string, size int64) (int64, int64, error) {
	value, found := strings.CutPrefix(strings.TrimSpace(spec), "bytes=")
	if !found {
		return 0, 0, fmt.Errorf("range %q", spec)
	}
	firstValue, lastValue, found := strings.Cut(value, "-")
	if !found {
		return 0, 0, fmt.Errorf("range %q", spec)
	}
	first, firstErr := strconv.ParseInt(firstValue, 10, 64)
	last, lastErr := strconv.ParseInt(lastValue, 10, 64)
	if firstErr != nil || lastErr != nil || first < 0 || last < first || last >= size {
		return 0, 0, fmt.Errorf("range %q", spec)
	}
	return first, last, nil
}

// fetchAll runs one download and returns the digest of what it delivered, so a
// comparison does not hold a second copy of the asset.
func fetchAll(t *testing.T, client *Client, url string) [32]byte {
	t.Helper()
	response, err := client.Fetch(context.Background(), application.FetchRequest{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	size, err := io.Copy(hash, response.Body)
	closeErr := response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	if size != response.Length {
		t.Fatalf("read %d bytes, Content-Length was %d", size, response.Length)
	}
	return [32]byte(hash.Sum(nil))
}

func TestFetchSplitsARangedAsset(t *testing.T) {
	origin := newRangeServer(assetSize)
	server := httptest.NewServer(origin)
	defer server.Close()
	client := New(Options{Timeout: 10 * time.Second, Parallelism: 4})
	defer client.Close()

	if got := fetchAll(t, client, server.URL); got != sha256.Sum256(origin.data) {
		t.Fatal("the assembled asset does not match the origin")
	}
	// One response carries the first chunk and the rest arrive as ranges.
	if parts := origin.parts.Load(); parts != 2 {
		t.Fatalf("the origin served %d ranges, expected 2", parts)
	}
	if origin.requests.Load() != 3 {
		t.Fatalf("the origin answered %d requests", origin.requests.Load())
	}
	if origin.matched.Load() != origin.parts.Load() {
		t.Fatalf("%d of %d ranges carried If-Match", origin.matched.Load(), origin.parts.Load())
	}
	if len(client.budget) != 0 {
		t.Fatalf("closing the body left %d slots taken", len(client.budget))
	}
}

func TestTransportDecoratorSeesRangeRequests(t *testing.T) {
	origin := newRangeServer(assetSize)
	server := httptest.NewServer(origin)
	defer server.Close()
	var requests atomic.Int32
	client := New(Options{
		Timeout:             10 * time.Second,
		Parallelism:         4,
		TransportDecorators: []TransportDecorator{countRequests(&requests)},
	})
	defer client.Close()

	fetchAll(t, client, server.URL)
	if int64(requests.Load()) != origin.requests.Load() || requests.Load() != 3 {
		t.Fatalf("decorated requests = %d, origin requests = %d", requests.Load(), origin.requests.Load())
	}
}

func TestFetchSplitsOnALastModifiedValidator(t *testing.T) {
	origin := newRangeServer(assetSize)
	origin.etag = ""
	origin.lastModified = time.Now().UTC().Add(-time.Hour).Format(http.TimeFormat)
	server := httptest.NewServer(origin)
	defer server.Close()
	client := New(Options{Timeout: 10 * time.Second, Parallelism: 4})
	defer client.Close()

	if got := fetchAll(t, client, server.URL); got != sha256.Sum256(origin.data) {
		t.Fatal("the assembled asset does not match the origin")
	}
	if parts := origin.parts.Load(); parts != 2 {
		t.Fatalf("the origin served %d ranges, expected 2", parts)
	}
}

// A split download reads one asset over several requests, so DAC declines to
// split what it cannot pin: an asset that changes mid-transfer would otherwise
// be reassembled out of two versions.
func TestFetchStreamsWhatItCannotSplit(t *testing.T) {
	cases := []struct {
		name        string
		parallelism int
		size        int
		prepare     func(*rangeServer)
	}{
		{name: "no parallelism", parallelism: 1, size: assetSize},
		{name: "no range support", parallelism: 4, size: assetSize, prepare: func(server *rangeServer) { server.acceptRanges = false }},
		{name: "no validator", parallelism: 4, size: assetSize, prepare: func(server *rangeServer) { server.etag = "" }},
		{name: "weak validator", parallelism: 4, size: assetSize, prepare: func(server *rangeServer) { server.etag = "W/\"v1\"" }},
		{name: "below the split size", parallelism: 4, size: minSplitSize - 1},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			origin := newRangeServer(test.size)
			if test.prepare != nil {
				test.prepare(origin)
			}
			server := httptest.NewServer(origin)
			defer server.Close()
			client := New(Options{Timeout: 10 * time.Second, Parallelism: test.parallelism})
			defer client.Close()

			if got := fetchAll(t, client, server.URL); got != sha256.Sum256(origin.data) {
				t.Fatal("the streamed asset does not match the origin")
			}
			if origin.requests.Load() != 1 {
				t.Fatalf("the origin answered %d requests, expected 1", origin.requests.Load())
			}
		})
	}
}

// A chunk has not reached the hash when it fails, so retrying it costs one
// chunk rather than the asset.
func TestFetchRetriesOneChunk(t *testing.T) {
	origin := newRangeServer(assetSize)
	origin.reject = func(count int64) int {
		if count == 1 {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	server := httptest.NewServer(origin)
	defer server.Close()
	client := New(Options{Timeout: 10 * time.Second, Retries: 2, Parallelism: 4})
	defer client.Close()

	if got := fetchAll(t, client, server.URL); got != sha256.Sum256(origin.data) {
		t.Fatal("the assembled asset does not match the origin")
	}
	if parts := origin.parts.Load(); parts != 3 {
		t.Fatalf("the origin served %d ranges, expected 3", parts)
	}
}

func TestFetchFailsWhenTheEntityChangesMidDownload(t *testing.T) {
	origin := newRangeServer(assetSize)
	origin.reject = func(int64) int { return http.StatusPreconditionFailed }
	server := httptest.NewServer(origin)
	defer server.Close()
	client := New(Options{Timeout: 10 * time.Second, Retries: 1, Parallelism: 4})
	defer client.Close()

	response, err := client.Fetch(context.Background(), application.FetchRequest{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if err == nil || !strings.Contains(err.Error(), "HTTP 412") {
		t.Fatalf("expected a precondition failure, got %v", err)
	}
	if len(client.budget) != 0 {
		t.Fatalf("a failed download left %d slots taken", len(client.budget))
	}
}

// A server that answers a range request with a different range would otherwise
// be caught only as a digest mismatch, which reports that the bytes were wrong
// without saying why.
func TestFetchRejectsAMisplacedRange(t *testing.T) {
	origin := newRangeServer(assetSize)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Range") == "" {
			origin.ServeHTTP(writer, request)
			return
		}
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes 0-1/%d", len(origin.data)))
		writer.Header().Set("Content-Length", "2")
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(origin.data[:2])
	}))
	defer server.Close()
	client := New(Options{Timeout: 10 * time.Second, Parallelism: 4})
	defer client.Close()

	response, err := client.Fetch(context.Background(), application.FetchRequest{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if err == nil || !strings.Contains(err.Error(), "for the range") {
		t.Fatalf("expected a range mismatch, got %v", err)
	}
}

// The bytes are hashed in order, so a chunk that arrives early waits in memory
// for its turn. A worker that could hand one over and move on regardless would
// read the whole asset into memory ahead of the reader, which is the cost the
// chunk size exists to bound.
func TestSplitWaitsForTheReader(t *testing.T) {
	// Five chunks and three workers, so a fourth chunk is only claimed once the
	// reader has taken one off a worker's hands.
	origin := newRangeServer(5 * chunkSize)
	server := httptest.NewServer(origin)
	defer server.Close()
	client := New(Options{Timeout: 10 * time.Second, Parallelism: 4})
	defer client.Close()

	response, err := client.Fetch(context.Background(), application.FetchRequest{URL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	// Read the chunk the first response carries and nothing after it.
	if _, err := io.CopyN(io.Discard, response.Body, chunkSize); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if parts := origin.parts.Load(); parts > 3 {
		t.Fatalf("the origin served %d ranges before the reader asked for one, expected at most 3", parts)
	}
}

// The parallelism is the whole client's, so a download takes what is free and
// streams the rest rather than waiting for a slot.
func TestSplitUsesWhatParallelismIsLeft(t *testing.T) {
	client := New(Options{Timeout: time.Second, Parallelism: 4})
	defer client.Close()
	if taken := client.acquire(3); taken != 3 {
		t.Fatalf("acquired %d slots, expected 3", taken)
	}
	if taken := client.acquire(3); taken != 1 {
		t.Fatalf("acquired %d slots of the one left, expected 1", taken)
	}
	if taken := client.acquire(1); taken != 0 {
		t.Fatalf("acquired %d slots of none left, expected 0", taken)
	}
	client.release(4)
	if len(client.budget) != 0 {
		t.Fatalf("%d slots stayed taken", len(client.budget))
	}
}

func TestContentRange(t *testing.T) {
	cases := []struct {
		value string
		first int64
		last  int64
		valid bool
	}{
		{value: "bytes 0-99/1000", first: 0, last: 99, valid: true},
		{value: "bytes 500-999/*", first: 500, last: 999, valid: true},
		{value: "  bytes  8-9/10  ", first: 8, last: 9, valid: true},
		{value: "items 0-99/1000"},
		{value: "bytes */1000"},
		{value: "bytes 0-99"},
		{value: "bytes x-99/1000"},
		{value: ""},
	}
	for _, test := range cases {
		first, last, err := contentRange(test.value)
		if test.valid && (err != nil || first != test.first || last != test.last) {
			t.Errorf("contentRange(%q) = %d, %d, %v", test.value, first, last, err)
		}
		if !test.valid && err == nil {
			t.Errorf("contentRange(%q) was accepted", test.value)
		}
	}
}
