package asset

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tomdoesdev/dac/internal/fault"
)

// TestDownloadAssemblesChunksInOrder is the core guarantee of ranged
// downloads: many requests produce exactly the bytes one request would have,
// in order, and the transfer is still one range at a time.
func TestDownloadAssemblesChunksInOrder(t *testing.T) {
	payload := testPayload(9000)
	host := newRangeHost(payload)
	server := httptest.NewServer(host)
	defer server.Close()

	download, err := downloadFrom(t, 2048, server.URL, "")
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	defer func() { _ = download.Discard() }()

	if download.Digest != digestOf(payload) || download.Size != int64(len(payload)) {
		t.Fatalf("Download() = %s (%d bytes), want %s (%d bytes)", download.Digest, download.Size, digestOf(payload), len(payload))
	}
	ranges := host.ranges()
	want := []string{"bytes=0-2047", "bytes=2048-4095", "bytes=4096-6143", "bytes=6144-8191", "bytes=8192-8999"}
	if strings.Join(ranges, " ") != strings.Join(want, " ") {
		t.Fatalf("requested ranges = %v, want %v", ranges, want)
	}
	// Every chunk after the first must carry the validator the first response
	// offered, or a host could satisfy it from a newer version of the asset.
	for index, validator := range host.validators()[1:] {
		if validator != testETag {
			t.Fatalf("chunk %d If-Range = %q, want %q", index+1, validator, testETag)
		}
	}
}

// TestDownloadFallsBackWhenRangesAreUnavailable keeps hosts that cannot serve
// ranges working. The probe carries a Range header, so a host that ignores it
// and one that rejects it outright are both ordinary downloads.
func TestDownloadFallsBackWhenRangesAreUnavailable(t *testing.T) {
	tests := []struct {
		name     string
		payload  []byte
		handler  func(payload []byte) http.Handler
		requests int
	}{
		{
			name:    "host ignores the range",
			payload: testPayload(5000),
			handler: func(payload []byte) http.Handler {
				return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
					writer.Header().Set("Content-Length", strconv.Itoa(len(payload)))
					_, _ = writer.Write(payload)
				})
			},
			requests: 1,
		},
		{
			name:    "host rejects the range",
			payload: nil,
			handler: func(payload []byte) http.Handler {
				// An empty asset is the case every host rejects: there is no
				// byte zero to satisfy the probe with.
				return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Header.Get(rangeHeader) != "" {
						writer.Header().Set(contentRangeHeader, "bytes */0")
						writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
						return
					}
					writer.Header().Set("Content-Length", strconv.Itoa(len(payload)))
					_, _ = writer.Write(payload)
				})
			},
			requests: 2,
		},
		{
			name:    "host serves an unusable Content-Range",
			payload: testPayload(3000),
			handler: func(payload []byte) http.Handler {
				return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					if request.Header.Get(rangeHeader) == "" {
						writer.Header().Set("Content-Length", strconv.Itoa(len(payload)))
						_, _ = writer.Write(payload)
						return
					}
					writer.Header().Set(contentRangeHeader, "bytes 0-1023/*")
					writer.Header().Set("Content-Length", "1024")
					writer.WriteHeader(http.StatusPartialContent)
					_, _ = writer.Write(payload[:1024])
				})
			},
			requests: 2,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests int
			var mutex sync.Mutex
			handler := test.handler(test.payload)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				mutex.Lock()
				requests++
				mutex.Unlock()
				handler.ServeHTTP(writer, request)
			}))
			defer server.Close()

			download, err := downloadFrom(t, 1024, server.URL, "")
			if err != nil {
				t.Fatalf("Download() error = %v", err)
			}
			defer func() { _ = download.Discard() }()
			if download.Digest != digestOf(test.payload) {
				t.Fatalf("Download() digest = %s, want %s", download.Digest, digestOf(test.payload))
			}
			if requests != test.requests {
				t.Fatalf("requests = %d, want %d", requests, test.requests)
			}
		})
	}
}

// TestDownloadResumesAfterATruncatedChunk is why chunking is worth its extra
// requests: a connection lost part-way costs the current chunk rather than
// every byte already hashed.
func TestDownloadResumesAfterATruncatedChunk(t *testing.T) {
	payload := testPayload(4096)
	host := newRangeHost(payload)
	host.truncate = map[int]bool{1: true}
	server := httptest.NewServer(host)
	defer server.Close()

	download, err := downloadFrom(t, 1024, server.URL, "")
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	defer func() { _ = download.Discard() }()
	if download.Digest != digestOf(payload) {
		t.Fatalf("Download() digest = %s, want %s", download.Digest, digestOf(payload))
	}
	// The resumed request starts at the first byte dac did not accept and asks
	// for a whole chunk from there, so no byte is ever requested twice.
	want := []string{"bytes=0-1023", "bytes=1024-2047", "bytes=1536-2559", "bytes=2560-3583", "bytes=3584-4095"}
	if got := host.ranges(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("requested ranges = %v, want %v", got, want)
	}
}

// TestDownloadRejectsAChangedResource covers the failure a resumed transfer
// must never paper over: bytes from two different versions of an asset would
// concatenate into a file that never existed anywhere.
func TestDownloadRejectsAChangedResource(t *testing.T) {
	payload := testPayload(4096)
	host := newRangeHost(payload)
	host.ignoreRangeAfter = 1
	server := httptest.NewServer(host)
	defer server.Close()

	_, err := downloadFrom(t, 1024, server.URL, "")
	if !errors.Is(err, ErrDownloadRangeMismatch) {
		t.Fatalf("Download() error = %v, want error matching %v", err, ErrDownloadRangeMismatch)
	}
	var operation *fault.Error
	if !errors.As(err, &operation) || operation.Kind() != fault.ErrorKindNetwork {
		t.Fatalf("Download() error kind = %v, want network", err)
	}
}

// TestChunkRequestsHonourTheRedirectTrustBoundary protects the shortcut that
// makes chunking cheap. Later chunks go straight to the location the first
// response resolved to, so they must apply the same rule a repeated redirect
// would have: configured headers stay behind at their own origin.
func TestChunkRequestsHonourTheRedirectTrustBoundary(t *testing.T) {
	payload := testPayload(3000)
	host := newRangeHost(payload)
	target := httptest.NewServer(host)
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusFound)
	}))
	defer origin.Close()

	downloader := newRangeDownloader(1024)
	downloads, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = downloads.Close() }()
	download, err := downloader.Download(context.Background(), downloads, Request{
		Name:    "asset",
		URL:     origin.URL,
		File:    "asset",
		Headers: map[string]string{"Authorization": "test-credential"},
		Policy:  DefaultTransferPolicy(),
	}, "")
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	defer func() { _ = download.Discard() }()

	if download.Digest != digestOf(payload) {
		t.Fatalf("Download() digest = %s, want %s", download.Digest, digestOf(payload))
	}
	if got := len(host.ranges()); got != 3 {
		t.Fatalf("chunk requests = %d, want 3", got)
	}
	for index, credential := range host.credentials() {
		if credential != "" {
			t.Fatalf("chunk %d sent Authorization %q across origins", index, credential)
		}
	}
}

// TestParseContentRange fixes the grammar dac relies on to place every later
// chunk, including the forms it must refuse rather than guess at.
func TestParseContentRange(t *testing.T) {
	tests := []struct {
		value                 string
		first, last, complete int64
		wantErr               bool
	}{
		{value: "bytes 0-1023/4096", first: 0, last: 1023, complete: 4096},
		{value: "bytes 4096-4096/4097", first: 4096, last: 4096, complete: 4097},
		{value: "bytes 0-1023/*", wantErr: true},
		{value: "bytes 1024-0/4096", wantErr: true},
		{value: "bytes 0-4096/4096", wantErr: true},
		{value: "items 0-1023/4096", wantErr: true},
		{value: "bytes 0-1023", wantErr: true},
		{value: "", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.value, func(t *testing.T) {
			first, last, complete, err := parseContentRange(test.value)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseContentRange(%q) = %d, %d, %d; want an error", test.value, first, last, complete)
				}
				return
			}
			if err != nil || first != test.first || last != test.last || complete != test.complete {
				t.Fatalf("parseContentRange(%q) = %d, %d, %d, %v; want %d, %d, %d", test.value, first, last, complete, err, test.first, test.last, test.complete)
			}
		})
	}
}

const testETag = `"chunk-test"`

// rangeHost serves one payload with byte-range support and records what each
// request asked for, so a test can assert the shape of a transfer rather than
// only its result. Individual chunks can be made to misbehave by index.
type rangeHost struct {
	payload []byte
	// truncate names chunk requests, by order of arrival, that abort after
	// half the bytes they promised.
	truncate map[int]bool
	// ignoreRangeAfter is the number of ranged requests served before the host
	// starts answering with the whole asset instead.
	ignoreRangeAfter int

	mutex     sync.Mutex
	requested []string
	validated []string
	sent      []string
}

func newRangeHost(payload []byte) *rangeHost {
	return &rangeHost{payload: payload, ignoreRangeAfter: -1}
}

func (host *rangeHost) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	index := host.record(request)
	writer.Header().Set("ETag", testETag)
	first, last, ranged := requestedRange(request, int64(len(host.payload)))
	if !ranged || (host.ignoreRangeAfter >= 0 && index >= host.ignoreRangeAfter) {
		writer.Header().Set("Content-Length", strconv.Itoa(len(host.payload)))
		_, _ = writer.Write(host.payload)
		return
	}
	writer.Header().Set(contentRangeHeader, fmt.Sprintf("bytes %d-%d/%d", first, last, len(host.payload)))
	writer.Header().Set("Content-Length", strconv.FormatInt(last-first+1, 10))
	writer.WriteHeader(http.StatusPartialContent)
	body := host.payload[first : last+1]
	if host.truncate[index] {
		_, _ = writer.Write(body[:len(body)/2])
		// The flush puts those bytes on the wire before the connection dies,
		// so the client keeps them and resumes from where they stopped rather
		// than asking for the whole chunk again.
		writer.(http.Flusher).Flush()
		// Abandoning a response that promised more bytes is what a dropped
		// connection looks like to the client.
		panic(http.ErrAbortHandler)
	}
	_, _ = writer.Write(body)
}

func (host *rangeHost) record(request *http.Request) int {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	host.requested = append(host.requested, request.Header.Get(rangeHeader))
	host.validated = append(host.validated, request.Header.Get(ifRangeHeader))
	host.sent = append(host.sent, request.Header.Get("Authorization"))
	return len(host.requested) - 1
}

func (host *rangeHost) ranges() []string      { return host.snapshot(host.requested) }
func (host *rangeHost) validators() []string  { return host.snapshot(host.validated) }
func (host *rangeHost) credentials() []string { return host.snapshot(host.sent) }

func (host *rangeHost) snapshot(values []string) []string {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	return append([]string(nil), values...)
}

// requestedRange reads the single closed range dac sends, which is the only
// form this host needs to understand.
func requestedRange(request *http.Request, size int64) (int64, int64, bool) {
	value, found := strings.CutPrefix(request.Header.Get(rangeHeader), "bytes=")
	if !found {
		return 0, 0, false
	}
	firstValue, lastValue, found := strings.Cut(value, "-")
	if !found {
		return 0, 0, false
	}
	first, firstErr := strconv.ParseInt(firstValue, 10, 64)
	last, lastErr := strconv.ParseInt(lastValue, 10, 64)
	if firstErr != nil || lastErr != nil || first >= size {
		return 0, 0, false
	}
	return first, min(last, size-1), true
}

// newRangeDownloader drives the multi-chunk path with chunks small enough for
// a test to enumerate.
func newRangeDownloader(chunkSize int64) *Downloader {
	downloader := NewDownloader(NewHTTPClient(), "dac/test")
	downloader.chunkSize = chunkSize
	return downloader
}

func downloadFrom(t *testing.T, chunkSize int64, url, expected string) (*StagedDownload, error) {
	t.Helper()
	downloads, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = downloads.Close() })
	return newRangeDownloader(chunkSize).Download(context.Background(), downloads, Request{
		Name: "asset", URL: url, File: "asset", Policy: DefaultTransferPolicy(),
	}, expected)
}

// testPayload produces bytes whose value depends on their offset, so chunks
// reassembled in the wrong order cannot still hash correctly.
func testPayload(size int) []byte {
	payload := make([]byte, size)
	for index := range payload {
		payload[index] = byte(index*7 + index/251)
	}
	return payload
}

func digestOf(payload []byte) string {
	sum := sha256.Sum256(payload)
	return formatSHA256Digest(sum[:])
}
