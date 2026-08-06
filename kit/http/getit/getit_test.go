package getit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testChunk is the chunk size the tests split at. It is small so that a test
// file of several chunks is still small.
const testChunk = 64 << 10

// fileServer serves one file, whole or as byte ranges, and records what it was
// asked for.
type fileServer struct {
	data         []byte
	etag         string
	lastModified string
	acceptRanges bool
	encoding     string
	// reject returns the status one whole-body request answers with, by the
	// order the server received it. A zero status serves the request.
	reject func(count int64) int
	// rejectRange does the same for range requests.
	rejectRange func(count int64) int
	// onRange runs at the start of each range request, before it is answered.
	onRange func()
	// stall writes part of the body and then stops, until the channel closes.
	stall chan struct{}

	requests atomic.Int64
	ranges   atomic.Int64

	mutex   sync.Mutex
	headers []http.Header
}

func newFileServer(size int) *fileServer {
	data := make([]byte, size)
	for index := range data {
		data[index] = byte(index*31 + index/1021)
	}
	return &fileServer{data: data, etag: `"v1"`, acceptRanges: true}
}

// start serves this file over a test server that ends with the test.
func (server *fileServer) start(t *testing.T) string {
	t.Helper()
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	return httpServer.URL + "/file.bin"
}

func (server *fileServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.requests.Add(1)
	server.record(request.Header)
	if server.acceptRanges {
		writer.Header().Set("Accept-Ranges", "bytes")
	}
	if server.etag != "" {
		writer.Header().Set("ETag", server.etag)
	}
	if server.lastModified != "" {
		writer.Header().Set("Last-Modified", server.lastModified)
	}
	if server.encoding != "" {
		writer.Header().Set("Content-Encoding", server.encoding)
	}
	if spec := request.Header.Get("Range"); spec != "" {
		server.serveRange(writer, spec)
		return
	}
	if server.unchanged(request) {
		writer.WriteHeader(http.StatusNotModified)
		return
	}
	if server.reject != nil {
		if status := server.reject(server.requests.Load()); status != 0 {
			writer.WriteHeader(status)
			return
		}
	}
	writer.Header().Set("Content-Length", strconv.Itoa(len(server.data)))
	writer.WriteHeader(http.StatusOK)
	if server.stall != nil {
		_, _ = writer.Write(server.data[:1])
		writer.(http.Flusher).Flush()
		<-server.stall
		return
	}
	_, _ = writer.Write(server.data)
}

func (server *fileServer) serveRange(writer http.ResponseWriter, spec string) {
	count := server.ranges.Add(1)
	if server.onRange != nil {
		server.onRange()
	}
	if server.rejectRange != nil {
		if status := server.rejectRange(count); status != 0 {
			writer.WriteHeader(status)
			return
		}
	}
	first, last, err := parseRange(spec, int64(len(server.data)))
	if err != nil {
		writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	writer.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(server.data)))
	writer.Header().Set("Content-Length", strconv.FormatInt(last-first+1, 10))
	writer.WriteHeader(http.StatusPartialContent)
	_, _ = writer.Write(server.data[first : last+1])
}

// unchanged reports whether a conditional request may be answered with a 304.
func (server *fileServer) unchanged(request *http.Request) bool {
	if tag := request.Header.Get("If-None-Match"); tag != "" && server.etag != "" {
		return tag == server.etag
	}
	if since := request.Header.Get("If-Modified-Since"); since != "" && server.lastModified != "" {
		return since == server.lastModified
	}
	return false
}

func (server *fileServer) record(header http.Header) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	server.headers = append(server.headers, header.Clone())
}

// seen returns the values one header carried, over every request so far.
func (server *fileServer) seen(name string) []string {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	values := make([]string, 0, len(server.headers))
	for _, header := range server.headers {
		values = append(values, header.Get(name))
	}
	return values
}

func parseRange(spec string, size int64) (int64, int64, error) {
	value, found := strings.CutPrefix(strings.TrimSpace(spec), "bytes=")
	if !found {
		return 0, 0, errors.New("not a byte range")
	}
	firstText, lastText, found := strings.Cut(value, "-")
	if !found {
		return 0, 0, errors.New("not a byte range")
	}
	first, err := strconv.ParseInt(firstText, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	if lastText == "" {
		return first, size - 1, nil
	}
	last, err := strconv.ParseInt(lastText, 10, 64)
	if err != nil {
		return 0, 0, err
	}
	if first > last || last >= size {
		return 0, 0, errors.New("out of range")
	}
	return first, last, nil
}

// fetch gets a URL and returns the body, closing it.
func fetch(t *testing.T, getter *Getter, target string, options ...RequestOption) ([]byte, *Response) {
	t.Helper()
	response, err := getter.Get(t.Context(), target, options...)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	return data, response
}

// read gets a URL and returns the body without failing the test, for the tests
// that fetch from a goroutine of their own.
func read(ctx context.Context, getter *Getter, target string, options ...RequestOption) ([]byte, error) {
	response, err := getter.Get(ctx, target, options...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	return io.ReadAll(response.Body)
}

func TestGetReadsTheWholeFile(t *testing.T) {
	server := newFileServer(3 * testChunk)
	getter := New(WithChunkSize(testChunk))
	defer getter.Close()

	data, response := fetch(t, getter, server.start(t))

	if !equal(data, server.data) {
		t.Fatalf("got %d bytes, want the %d the server holds", len(data), len(server.data))
	}
	if response.Status != http.StatusOK {
		t.Errorf("status is %d, want 200", response.Status)
	}
	if response.Length != int64(len(server.data)) {
		t.Errorf("length is %d, want %d", response.Length, len(server.data))
	}
	if response.Validator.ETag != `"v1"` {
		t.Errorf(`etag is %q, want "v1"`, response.Validator.ETag)
	}
	// Nothing asked for parts, so nothing should have been split.
	if server.ranges.Load() != 0 {
		t.Errorf("the server saw %d range requests, want 0", server.ranges.Load())
	}
}

func TestGetSendsIdentityEncoding(t *testing.T) {
	server := newFileServer(testChunk)
	getter := New()
	defer getter.Close()

	fetch(t, getter, server.start(t))

	if got := server.seen("Accept-Encoding"); got[0] != "identity" {
		t.Errorf("Accept-Encoding is %q, want identity", got[0])
	}
}

func TestGetRefusesANonIdentityEncoding(t *testing.T) {
	server := newFileServer(testChunk)
	server.encoding = "gzip"
	getter := New()
	defer getter.Close()

	_, err := getter.Get(t.Context(), server.start(t))

	if err == nil {
		t.Fatal("a gzip body was accepted, want a refusal")
	}
	if !strings.Contains(err.Error(), "identity") {
		t.Errorf("error is %v, want one naming the encoding", err)
	}
	// The refusal is permanent, so it is made once.
	if server.requests.Load() != 1 {
		t.Errorf("the server saw %d requests, want 1", server.requests.Load())
	}
}

func TestGetCarriesCallerHeaders(t *testing.T) {
	server := newFileServer(testChunk)
	getter := New()
	defer getter.Close()

	fetch(t, getter, server.start(t), WithHeader("Authorization", "Bearer token"))

	if got := server.seen("Authorization"); got[0] != "Bearer token" {
		t.Errorf("Authorization is %q, want the value the caller set", got[0])
	}
}

// A header map cloned per call is what stops one Get from writing into another.
func TestCallerHeadersDoNotLeakBetweenCalls(t *testing.T) {
	server := newFileServer(testChunk)
	getter := New(WithDefaults(WithHeader("X-Common", "shared")))
	defer getter.Close()
	target := server.start(t)

	fetch(t, getter, target, WithHeader("X-Only-Once", "yes"))
	fetch(t, getter, target)

	common := server.seen("X-Common")
	if common[0] != "shared" || common[1] != "shared" {
		t.Errorf("X-Common is %q, want it on both requests", common)
	}
	once := server.seen("X-Only-Once")
	if once[0] != "yes" || once[1] != "" {
		t.Errorf("X-Only-Once is %q, want it on the first request only", once)
	}
}

func TestGetRefusesAURLThePolicyRejects(t *testing.T) {
	getter := New(WithURLPolicy(func(value *url.URL) error {
		if value.Scheme != "https" {
			return fmt.Errorf("%w: HTTPS only", ErrNotPermitted)
		}
		return nil
	}))
	defer getter.Close()

	_, err := getter.Get(t.Context(), "http://example.invalid/file.bin")

	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("error is %v, want one matching ErrNotPermitted", err)
	}
	if !errors.Is(err, ErrPermanent) {
		t.Error("a policy refusal should be permanent, so it is not retried")
	}
}

func TestDefaultPolicyRefusesCredentialsInTheURL(t *testing.T) {
	getter := New()
	defer getter.Close()

	_, err := getter.Get(t.Context(), "https://user:secret@example.invalid/file.bin")

	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("error is %v, want one matching ErrNotPermitted", err)
	}
}

func TestThePolicyAppliesToEveryRedirect(t *testing.T) {
	server := newFileServer(testChunk)
	target := server.start(t)
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target, http.StatusFound)
	}))
	defer redirect.Close()

	reached := map[string]bool{}
	var mutex sync.Mutex
	getter := New(WithURLPolicy(func(value *url.URL) error {
		mutex.Lock()
		defer mutex.Unlock()
		reached[value.Host] = true
		if value.Host != strings.TrimPrefix(redirect.URL, "http://") {
			return fmt.Errorf("%w: %s", ErrNotPermitted, value.Host)
		}
		return nil
	}))
	defer getter.Close()

	_, err := getter.Get(t.Context(), redirect.URL+"/file.bin")

	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("error is %v, want the policy to refuse the redirect target", err)
	}
	if len(reached) != 2 {
		t.Errorf("the policy saw %d hosts, want the first URL and the redirect target", len(reached))
	}
}

func TestGetRefusesABodyOverTheLimit(t *testing.T) {
	server := newFileServer(4 * testChunk)
	getter := New()
	defer getter.Close()

	_, err := getter.Get(t.Context(), server.start(t), WithMaxBytes(testChunk))

	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error is %v, want one matching ErrTooLarge", err)
	}
	// The declared length answered this, so no body was read.
	if server.requests.Load() != 1 {
		t.Errorf("the server saw %d requests, want 1", server.requests.Load())
	}
}

// An origin that declares no length, or declares one it then exceeds, is caught
// by counting the bytes that actually arrive.
func TestTheLimitAppliesToBytesThatArrive(t *testing.T) {
	data := make([]byte, 4*testChunk)
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(data)
	}))
	defer httpServer.Close()
	getter := New()
	defer getter.Close()

	response, err := getter.Get(t.Context(), httpServer.URL+"/file.bin", WithMaxBytes(testChunk))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.Length != Unknown {
		t.Errorf("length is %d, want Unknown for a chunked response", response.Length)
	}

	_, err = io.ReadAll(response.Body)

	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("error is %v, want one matching ErrTooLarge", err)
	}
}

func TestGetFailsWithARequestError(t *testing.T) {
	server := newFileServer(testChunk)
	server.reject = func(int64) int { return http.StatusNotFound }
	getter := New()
	defer getter.Close()
	target := server.start(t)

	_, err := getter.Get(t.Context(), target)

	var failure *RequestError
	if !errors.As(err, &failure) {
		t.Fatalf("error is %T, want a *RequestError", err)
	}
	if failure.Status != http.StatusNotFound {
		t.Errorf("status is %d, want 404", failure.Status)
	}
	if failure.URL != target {
		t.Errorf("URL is %q, want %q", failure.URL, target)
	}
}

func TestBodyStallsAreReportedAsStalls(t *testing.T) {
	server := newFileServer(4 * testChunk)
	server.stall = make(chan struct{})
	defer close(server.stall)
	getter := New()
	defer getter.Close()

	response, err := getter.Get(t.Context(), server.start(t), WithStallTimeout(100*time.Millisecond))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	_, err = io.ReadAll(response.Body)

	if !errors.Is(err, ErrStalled) {
		t.Fatalf("error is %v, want one matching ErrStalled", err)
	}
}

func equal(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
