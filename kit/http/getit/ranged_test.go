package getit

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// splitSize is a file of eight chunks, which is enough to hand work to several
// parts and still leave chunks over for them to come back for.
const splitSize = 8 * testChunk

const lastModified = "Wed, 21 Oct 2015 07:28:00 GMT"

// splitGetter builds a Getter that splits, at the small test chunk size.
func splitGetter(parts int, options ...Option) *Getter {
	return New(append([]Option{
		WithChunkSize(testChunk),
		WithDefaults(WithParts(parts)),
	}, options...)...)
}

func TestSplitDownloadArrivesInOrder(t *testing.T) {
	server := newFileServer(splitSize)
	getter := splitGetter(4)
	defer getter.Close()

	data, response := fetch(t, getter, server.start(t))

	if !equal(data, server.data) {
		t.Fatal("the file did not reassemble to the bytes the server holds")
	}
	if response.Length != int64(splitSize) {
		t.Errorf("length is %d, want %d", response.Length, splitSize)
	}
	// The first response carries chunk 0, so the seven chunks after it are ranges.
	if got := server.ranges.Load(); got != 7 {
		t.Errorf("the server saw %d range requests, want 7", got)
	}
	if got := server.requests.Load(); got != 8 {
		t.Errorf("the server saw %d requests, want 8", got)
	}
}

func TestEveryRangeCarriesTheStrongEntityTag(t *testing.T) {
	server := newFileServer(splitSize)
	getter := splitGetter(4)
	defer getter.Close()

	fetch(t, getter, server.start(t))

	matches := 0
	for _, value := range server.seen("If-Match") {
		if value == `"v1"` {
			matches++
		}
	}
	if matches != 7 {
		t.Errorf("%d ranges carried If-Match, want all 7", matches)
	}
}

// An origin with no entity tag can still hold a split together with a date, and
// the date this package sends back is the one it was given.
func TestRangesFallBackToTheModificationDate(t *testing.T) {
	server := newFileServer(splitSize)
	server.etag = ""
	server.lastModified = lastModified
	getter := splitGetter(4)
	defer getter.Close()

	data, _ := fetch(t, getter, server.start(t))

	if !equal(data, server.data) {
		t.Fatal("the file did not reassemble to the bytes the server holds")
	}
	matches := 0
	for _, value := range server.seen("If-Unmodified-Since") {
		if value == lastModified {
			matches++
		}
	}
	if matches != 7 {
		t.Errorf("%d ranges carried If-Unmodified-Since, want all 7", matches)
	}
}

// A split is a thing this package does when it can. Each of these is a reason
// it cannot, and every one of them still delivers the whole file.
func TestSplitRefusals(t *testing.T) {
	cases := []struct {
		name    string
		parts   int
		options []RequestOption
		prepare func(*fileServer)
	}{
		{name: "one part", parts: 1},
		{name: "splitting is off", parts: 4, options: []RequestOption{WithoutSplit()}},
		{
			name:    "origin does not serve ranges",
			parts:   4,
			prepare: func(server *fileServer) { server.acceptRanges = false },
		},
		{
			// A weak tag says two responses are equivalent, not that they are the
			// same bytes, which is the only question a byte range asks.
			name:    "only a weak entity tag",
			parts:   4,
			prepare: func(server *fileServer) { server.etag = `W/"v1"` },
		},
		{
			name:    "no validator at all",
			parts:   4,
			prepare: func(server *fileServer) { server.etag = "" },
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			server := newFileServer(splitSize)
			if test.prepare != nil {
				test.prepare(server)
			}
			getter := splitGetter(test.parts)
			defer getter.Close()

			data, _ := fetch(t, getter, server.start(t), test.options...)

			if !equal(data, server.data) {
				t.Fatal("the file did not arrive whole")
			}
			if got := server.ranges.Load(); got != 0 {
				t.Errorf("the server saw %d range requests, want 0", got)
			}
		})
	}
}

func TestAFileBelowTwoChunksDoesNotSplit(t *testing.T) {
	server := newFileServer(2*testChunk - 1)
	getter := splitGetter(4)
	defer getter.Close()

	data, _ := fetch(t, getter, server.start(t))

	if !equal(data, server.data) {
		t.Fatal("the file did not arrive whole")
	}
	if got := server.ranges.Load(); got != 0 {
		t.Errorf("the server saw %d range requests, want 0", got)
	}
}

// A response with no declared length cannot be divided into ranges, so it
// streams over the one response instead.
func TestAResponseWithNoLengthDoesNotSplit(t *testing.T) {
	data := make([]byte, splitSize)
	for index := range data {
		data[index] = byte(index)
	}
	ranges := 0
	var mutex sync.Mutex
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Range") != "" {
			mutex.Lock()
			ranges++
			mutex.Unlock()
		}
		writer.Header().Set("Accept-Ranges", "bytes")
		writer.Header().Set("ETag", `"v1"`)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(data)
	}))
	defer httpServer.Close()
	getter := splitGetter(4)
	defer getter.Close()

	got, response := fetch(t, getter, httpServer.URL+"/file.bin")

	if !equal(got, data) {
		t.Fatal("the file did not arrive whole")
	}
	if response.Length != Unknown {
		t.Errorf("length is %d, want Unknown", response.Length)
	}
	if ranges != 0 {
		t.Errorf("the server saw %d range requests, want 0", ranges)
	}
}

// A range answered with the wrong bytes is refused rather than written into the
// file at the offset that was asked for.
func TestAWrongRangeIsRefused(t *testing.T) {
	data := make([]byte, splitSize)
	httpServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Accept-Ranges", "bytes")
		writer.Header().Set("ETag", `"v1"`)
		if request.Header.Get("Range") == "" {
			writer.Header().Set("Content-Length", strconv.Itoa(len(data)))
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(data)
			return
		}
		// Always the first chunk, whatever was asked for.
		writer.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", testChunk-1, len(data)))
		writer.WriteHeader(http.StatusPartialContent)
		_, _ = writer.Write(data[:testChunk])
	}))
	defer httpServer.Close()
	getter := splitGetter(4)
	defer getter.Close()

	response, err := getter.Get(t.Context(), httpServer.URL+"/file.bin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	_, err = io.ReadAll(response.Body)

	if err == nil {
		t.Fatal("a range answered with the wrong bytes was accepted")
	}
	if !strings.Contains(err.Error(), "for the range") {
		t.Errorf("error is %v, want one naming the range that was asked for", err)
	}
}

// The budget is what stops the parts of a transfer and the number of transfers
// from multiplying into more connections than the caller meant to open.
func TestTheBudgetBoundsRangeRequests(t *testing.T) {
	const budget = 3
	server := newFileServer(splitSize)
	started := make(chan struct{}, 32)
	release := make(chan struct{})
	server.onRange = func() {
		started <- struct{}{}
		<-release
	}
	getter := splitGetter(8, WithBudget(budget))
	defer getter.Close()

	target := server.start(t)
	done := make(chan []byte, 1)
	go func() {
		data, err := read(t.Context(), getter, target)
		if err != nil {
			t.Errorf("reading the file: %v", err)
		}
		done <- data
	}()

	for range budget {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("the download did not reach the budget")
		}
	}
	select {
	case <-started:
		t.Fatalf("a range request went out past a budget of %d", budget)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)

	data := <-done
	if !equal(data, server.data) {
		t.Fatal("the file did not reassemble to the bytes the server holds")
	}
	// Every part borrowed goes back, or the next transfer runs short of them.
	if held := len(getter.budget); held != 0 {
		t.Errorf("%d parts are still held after the body closed, want 0", held)
	}
}

// A transfer that can borrow no parts streams the body over its first response
// rather than waiting for the parts another transfer is holding.
func TestATransferWithNoPartsToSpareStreams(t *testing.T) {
	server := newFileServer(splitSize)
	started := make(chan struct{}, 32)
	release := make(chan struct{})
	server.onRange = func() {
		started <- struct{}{}
		<-release
	}
	getter := splitGetter(4, WithBudget(1))
	defer getter.Close()
	target := server.start(t)

	// The first transfer takes the only part there is, and holds on to it.
	first := make(chan struct{})
	go func() {
		defer close(first)
		if _, err := read(t.Context(), getter, target); err != nil {
			t.Errorf("the first transfer failed: %v", err)
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the first transfer never asked for a range")
	}
	held := server.ranges.Load()

	data, _ := fetch(t, getter, target)

	if !equal(data, server.data) {
		t.Fatal("the second transfer did not arrive whole")
	}
	if got := server.ranges.Load(); got != held {
		t.Errorf("the second transfer made %d range requests, want 0", got-held)
	}
	close(release)
	<-first
}

// One Getter is shared, so the parts of one transfer must not be read or
// written by another.
func TestConcurrentTransfersAreIndependent(t *testing.T) {
	server := newFileServer(splitSize)
	getter := splitGetter(4)
	defer getter.Close()
	target := server.start(t)

	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			response, err := getter.Get(t.Context(), target)
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			defer func() { _ = response.Body.Close() }()
			data, err := io.ReadAll(response.Body)
			if err != nil {
				t.Errorf("reading the body: %v", err)
				return
			}
			if !equal(data, server.data) {
				t.Error("a transfer did not reassemble to the bytes the server holds")
			}
		})
	}
	group.Wait()

	if held := len(getter.budget); held != 0 {
		t.Errorf("%d parts are still held, want 0", held)
	}
}

// Closing a body early must release the parts it borrowed and stop its workers,
// or a caller that gives up on one transfer starves the next.
func TestClosingEarlyReleasesTheParts(t *testing.T) {
	server := newFileServer(splitSize)
	getter := splitGetter(4)
	defer getter.Close()

	response, err := getter.Get(t.Context(), server.start(t))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	buffer := make([]byte, 16)
	if _, err := io.ReadFull(response.Body, buffer); err != nil {
		t.Fatalf("reading the first bytes: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("closing the body: %v", err)
	}

	if held := len(getter.budget); held != 0 {
		t.Errorf("%d parts are still held after an early close, want 0", held)
	}
}

// Each range is retried on its own, because a chunk that failed has been read
// by nobody yet.
func TestOneRangeIsRetriedOnItsOwn(t *testing.T) {
	server := newFileServer(splitSize)
	server.rejectRange = func(count int64) int {
		if count == 1 {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	getter := splitGetter(4)
	defer getter.Close()

	data, _ := fetch(t, getter, server.start(t))

	if !equal(data, server.data) {
		t.Fatal("the file did not reassemble to the bytes the server holds")
	}
	// Seven chunks, and the one that was rejected asked again.
	if got := server.ranges.Load(); got != 8 {
		t.Errorf("the server saw %d range requests, want 8", got)
	}
}

func TestARangeThatKeepsFailingFailsTheTransfer(t *testing.T) {
	server := newFileServer(splitSize)
	server.rejectRange = func(int64) int { return http.StatusInternalServerError }
	getter := splitGetter(4)
	defer getter.Close()

	response, err := getter.Get(t.Context(), server.start(t))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	_, err = io.ReadAll(response.Body)

	var failure *RequestError
	if !errors.As(err, &failure) {
		t.Fatalf("error is %v, want a *RequestError", err)
	}
	if failure.Status != http.StatusInternalServerError {
		t.Errorf("status is %d, want 500", failure.Status)
	}
}
