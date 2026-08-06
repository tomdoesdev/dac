package getit

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// recorder collects what the hooks reported. Every field is guarded, because
// the parts of a split download report at the same time as each other, which is
// the contract this is here to hold this package to.
type recorder struct {
	mutex     sync.Mutex
	starts    []Transfer
	ends      []Transfer
	endErrors []error
	requests  []Request
	responses []int
	retries   []Request
	// bytes is the sum of every progress report, and largest is the largest
	// running total any of them carried.
	bytes    int64
	largest  int64
	progress int
	parts    map[int]int64
}

func newRecorder() *recorder { return &recorder{parts: map[int]int64{}} }

func (record *recorder) hooks() Hooks {
	return Hooks{
		OnStart: func(transfer Transfer) {
			record.mutex.Lock()
			defer record.mutex.Unlock()
			record.starts = append(record.starts, transfer)
		},
		OnRequest: func(request Request) {
			record.mutex.Lock()
			defer record.mutex.Unlock()
			record.requests = append(record.requests, request)
		},
		OnResponse: func(_ Request, status int, _ error) {
			record.mutex.Lock()
			defer record.mutex.Unlock()
			record.responses = append(record.responses, status)
		},
		OnProgress: func(progress Progress) {
			record.mutex.Lock()
			defer record.mutex.Unlock()
			record.bytes += progress.Bytes
			record.largest = max(record.largest, progress.Received)
			record.parts[progress.Part] += progress.Bytes
			record.progress++
		},
		OnRetry: func(request Request, _ error, _ time.Duration) {
			record.mutex.Lock()
			defer record.mutex.Unlock()
			record.retries = append(record.retries, request)
		},
		OnEnd: func(transfer Transfer, err error) {
			record.mutex.Lock()
			defer record.mutex.Unlock()
			record.ends = append(record.ends, transfer)
			record.endErrors = append(record.endErrors, err)
		},
	}
}

func TestHooksReportASplitDownload(t *testing.T) {
	server := newFileServer(splitSize)
	getter := splitGetter(4)
	defer getter.Close()
	record := newRecorder()

	data, _ := fetch(t, getter, server.start(t), WithHooks(record.hooks()))

	if !equal(data, server.data) {
		t.Fatal("the file did not reassemble to the bytes the server holds")
	}
	if len(record.starts) != 1 || len(record.ends) != 1 {
		t.Fatalf("got %d starts and %d ends, want one of each", len(record.starts), len(record.ends))
	}
	if err := record.endErrors[0]; err != nil {
		t.Errorf("the transfer ended with %v, want nil", err)
	}
	transfer := record.starts[0]
	if transfer.Parts != 8 {
		t.Errorf("parts is %d, want the 8 chunks the file is in", transfer.Parts)
	}
	if transfer.Length != int64(splitSize) {
		t.Errorf("length is %d, want %d", transfer.Length, splitSize)
	}
	if transfer.ID != record.ends[0].ID {
		t.Error("the start and the end report different transfers")
	}
	// One request for the whole body, and one for each chunk after the first.
	if len(record.requests) != 8 {
		t.Errorf("got %d requests, want 8", len(record.requests))
	}
	if len(record.responses) != 8 {
		t.Errorf("got %d responses, want 8", len(record.responses))
	}
	if record.bytes != int64(splitSize) {
		t.Errorf("the progress reports add up to %d bytes, want %d", record.bytes, splitSize)
	}
	if record.largest != int64(splitSize) {
		t.Errorf("the largest running total is %d, want %d", record.largest, splitSize)
	}
	// Each chunk is reported against the request that carried it.
	if len(record.parts) != 8 {
		t.Errorf("progress named %d parts, want 8", len(record.parts))
	}
	for part, count := range record.parts {
		if count != int64(testChunk) {
			t.Errorf("part %d reported %d bytes, want %d", part, count, testChunk)
		}
	}
}

func TestHooksNameEveryRange(t *testing.T) {
	server := newFileServer(splitSize)
	getter := splitGetter(4)
	defer getter.Close()
	record := newRecorder()

	fetch(t, getter, server.start(t), WithHooks(record.hooks()))

	seen := map[int]Request{}
	for _, request := range record.requests {
		seen[request.Part] = request
	}
	if whole := seen[0]; whole.Start != Unknown || whole.End != Unknown {
		t.Errorf("the first request reports the range %d-%d, want Unknown", whole.Start, whole.End)
	}
	for part := 1; part < 8; part++ {
		request, found := seen[part]
		if !found {
			t.Errorf("no request was reported for part %d", part)
			continue
		}
		wantStart := int64(part) * testChunk
		if request.Start != wantStart || request.End != wantStart+testChunk-1 {
			t.Errorf("part %d reports the range %d-%d, want %d-%d",
				part, request.Start, request.End, wantStart, wantStart+testChunk-1)
		}
		if request.Attempt != 1 {
			t.Errorf("part %d reports attempt %d, want 1", part, request.Attempt)
		}
	}
}

func TestHooksReportAnUnsplitDownload(t *testing.T) {
	server := newFileServer(3 * testChunk)
	getter := New(WithChunkSize(testChunk))
	defer getter.Close()
	record := newRecorder()

	fetch(t, getter, server.start(t), WithHooks(record.hooks()))

	if len(record.starts) != 1 || record.starts[0].Parts != 1 {
		t.Errorf("got %d starts with parts %v, want one with 1 part", len(record.starts), record.starts)
	}
	if record.bytes != 3*testChunk {
		t.Errorf("the progress reports add up to %d bytes, want %d", record.bytes, 3*testChunk)
	}
	if len(record.parts) != 1 {
		t.Errorf("progress named %d parts, want 1", len(record.parts))
	}
}

// A Getter's hooks and one call's hooks both fire, so a trace set once and a
// meter set per transfer can live together.
func TestGetterHooksAndCallHooksBothFire(t *testing.T) {
	server := newFileServer(testChunk)
	shared, single := newRecorder(), newRecorder()
	getter := New(WithDefaults(WithHooks(shared.hooks())))
	defer getter.Close()
	target := server.start(t)

	fetch(t, getter, target, WithHooks(single.hooks()))
	fetch(t, getter, target)

	if len(shared.starts) != 2 {
		t.Errorf("the Getter's hooks saw %d transfers, want 2", len(shared.starts))
	}
	if len(single.starts) != 1 {
		t.Errorf("the call's hooks saw %d transfers, want 1", len(single.starts))
	}
	if shared.starts[0].ID == shared.starts[1].ID {
		t.Error("two transfers were reported under the same ID")
	}
}

func TestHooksReportARetry(t *testing.T) {
	server := newFileServer(testChunk)
	server.reject = func(count int64) int {
		if count == 1 {
			return http.StatusServiceUnavailable
		}
		return 0
	}
	getter := New()
	defer getter.Close()
	record := newRecorder()

	fetch(t, getter, server.start(t), WithHooks(record.hooks()))

	if len(record.retries) != 1 {
		t.Fatalf("got %d retries, want 1", len(record.retries))
	}
	if record.retries[0].Attempt != 1 {
		t.Errorf("the retry names attempt %d, want the first one", record.retries[0].Attempt)
	}
	if len(record.responses) != 2 || record.responses[0] != http.StatusServiceUnavailable {
		t.Errorf("responses are %v, want a 503 and then a 200", record.responses)
	}
	// The retry was a second try at the same request, not a second part.
	if len(record.requests) != 2 {
		t.Errorf("got %d requests, want 2", len(record.requests))
	}
	if record.requests[1].Attempt != 2 {
		t.Errorf("the second request names attempt %d, want 2", record.requests[1].Attempt)
	}
}

// A Get that never had a response to return reports neither a start nor an end.
// The failure is what Get returned, and a report could add nothing to it.
func TestAFailedGetReportsNoTransfer(t *testing.T) {
	server := newFileServer(testChunk)
	server.reject = func(int64) int { return http.StatusNotFound }
	getter := New()
	defer getter.Close()
	record := newRecorder()

	if _, err := getter.Get(t.Context(), server.start(t), WithHooks(record.hooks())); err == nil {
		t.Fatal("a 404 was accepted")
	}

	if len(record.starts) != 0 || len(record.ends) != 0 {
		t.Errorf("got %d starts and %d ends, want none of either", len(record.starts), len(record.ends))
	}
	// The request itself is still reported, because it was made.
	if len(record.requests) != 1 || len(record.responses) != 1 {
		t.Errorf("got %d requests and %d responses, want one of each", len(record.requests), len(record.responses))
	}
	if record.responses[0] != http.StatusNotFound {
		t.Errorf("the response reports %d, want 404", record.responses[0])
	}
}

// A body that failed part way carries the failure to the end report, so a meter
// can mark the transfer rather than leave it running.
func TestTheEndReportCarriesAFailure(t *testing.T) {
	server := newFileServer(splitSize)
	server.rejectRange = func(int64) int { return http.StatusInternalServerError }
	getter := splitGetter(4)
	defer getter.Close()
	record := newRecorder()

	response, err := getter.Get(t.Context(), server.start(t), WithHooks(record.hooks()))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_, readErr := io.ReadAll(response.Body)
	if readErr == nil {
		t.Fatal("a download whose ranges all failed was accepted")
	}
	_ = response.Body.Close()

	if len(record.ends) != 1 {
		t.Fatalf("got %d ends, want 1", len(record.ends))
	}
	var failure *RequestError
	if !errors.As(record.endErrors[0], &failure) {
		t.Fatalf("the end reports %v, want the failure that ended it", record.endErrors[0])
	}
}

func TestTheEndIsReportedOnce(t *testing.T) {
	server := newFileServer(testChunk)
	getter := New()
	defer getter.Close()
	record := newRecorder()

	response, err := getter.Get(t.Context(), server.start(t), WithHooks(record.hooks()))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_, _ = io.ReadAll(response.Body)
	for range 3 {
		_ = response.Body.Close()
	}

	if len(record.ends) != 1 {
		t.Errorf("got %d ends after three closes, want 1", len(record.ends))
	}
}

// A not-modified response transfers nothing, and still has a body to close and
// an end to report.
func TestANotModifiedResponseIsReported(t *testing.T) {
	server := newFileServer(testChunk)
	getter := New()
	defer getter.Close()
	record := newRecorder()

	response, err := getter.Get(t.Context(), server.start(t),
		WithValidator(Validator{ETag: `"v1"`}), WithHooks(record.hooks()))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !response.NotModified {
		t.Fatal("the response is not marked as unchanged")
	}
	_ = response.Body.Close()

	if len(record.starts) != 1 || len(record.ends) != 1 {
		t.Fatalf("got %d starts and %d ends, want one of each", len(record.starts), len(record.ends))
	}
	if record.starts[0].Parts != 0 {
		t.Errorf("parts is %d, want 0 for a transfer that carries nothing", record.starts[0].Parts)
	}
	if record.progress != 0 {
		t.Errorf("got %d progress reports, want none", record.progress)
	}
}
