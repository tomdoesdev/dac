package httpclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tomdoesdev/dac/internal/application"
)

// chunkSize is the span one range request asks for.
//
// It is a fixed size rather than a share of the asset, because the store hashes
// bytes in order, so a chunk that arrives early waits in memory for its turn.
// Splitting a 10GiB asset four ways would buy parallelism at the price of
// holding gigabytes; splitting it into 8MiB pieces that four workers take in
// turn keeps the transfer just as parallel with a bounded cost.
const chunkSize = 8 << 20

// minSplitSize is the smallest asset DAC splits. Below it the extra requests
// cost more in round trips than the parallelism returns.
const minSplitSize = 2 * chunkSize

// body returns the reader that delivers one response's bytes.
//
// It splits the transfer across parallel range requests when the origin permits
// it and the asset is large enough to pay for them, and otherwise hands back
// the single stream the response already carries.
func (client *Client) body(ctx context.Context, input application.FetchRequest, response *http.Response, cancel, cancelHead context.CancelFunc) io.ReadCloser {
	trace := client.options.Logger
	precondition, refused := splittable(client.options.Parallelism, response)
	if refused != "" {
		trace.Debug("streaming one response", "url", input.URL, "reason", refused, "length", response.ContentLength)
		return guard(response.Body, cancel, client.options.Timeout)
	}
	// The head response already carries the first chunk, so only the chunks
	// after it need a worker. A client that has already lent its parallelism to
	// other assets may have none left, in which case this download streams as it
	// always did rather than waiting for a slot.
	chunks := int((response.ContentLength + chunkSize - 1) / chunkSize)
	workers := client.acquire(min(client.options.Parallelism-1, chunks-1))
	if workers == 0 {
		trace.Debug("streaming one response", "url", input.URL, "reason", "no parts to spare", "length", response.ContentLength)
		return guard(response.Body, cancel, client.options.Timeout)
	}
	trace.Debug("splitting download", "url", input.URL, "length", response.ContentLength,
		"chunks", chunks, "workers", workers, "precondition", precondition.name)
	return client.split(ctx, input, response, cancel, cancelHead, precondition, chunks, workers)
}

// header is one request header, as a name and a value.
type header struct{ name, value string }

// splittable reports whether the rest of a response can be fetched as range
// requests, returning the precondition header that pins the entity while it is,
// or the reason it cannot be.
//
// The reason is what a trace needs. Every condition here is something the
// origin decided, so an operator watching a large asset arrive over one
// connection is looking at a server's answer rather than at a setting they got
// wrong, and only naming which one turns that into something they can act on.
//
// A split download reads one asset over several requests, so it has to be sure
// every request answers about the same bytes. The origin makes that promise: a
// precondition it can no longer satisfy is a 412 rather than a chunk of a newer
// file. Without a validator to build a precondition from, DAC does not split at
// all. Silently reassembling two versions of an asset would surface as a digest
// mismatch for a pinned asset, and for an unpinned one it would be locked as if
// it were a real object.
func splittable(parallelism int, response *http.Response) (header, string) {
	switch {
	case parallelism < 2:
		return header{}, "download-parts is 1"
	case response.StatusCode != http.StatusOK:
		return header{}, "response is not 200"
	case response.ContentLength < 0:
		return header{}, "response has no length"
	case response.ContentLength < minSplitSize:
		return header{}, "response is below the split size"
	}
	if !acceptsRanges(response.Header.Get("Accept-Ranges")) {
		return header{}, "origin does not serve byte ranges"
	}
	// A weak ETag says two responses are equivalent, not that they are the same
	// bytes, which is the only question a byte range asks.
	if etag := strings.TrimSpace(response.Header.Get("ETag")); etag != "" && !strings.HasPrefix(etag, "W/") {
		return header{name: "If-Match", value: etag}, ""
	}
	if modified := strings.TrimSpace(response.Header.Get("Last-Modified")); modified != "" {
		return header{name: "If-Unmodified-Since", value: modified}, ""
	}
	return header{}, "origin sent no strong validator"
}

func acceptsRanges(value string) bool {
	for token := range strings.SplitSeq(value, ",") {
		if strings.EqualFold(strings.TrimSpace(token), "bytes") {
			return true
		}
	}
	return false
}

// splitBody reassembles a split download in order.
//
// Order is not a convenience here: the caller hashes these bytes as it writes
// them, so a chunk that finishes early holds its buffer until the chunks before
// it have been read. That is what bounds the memory a split download costs.
// Each worker holds at most one chunk, so the peak is the worker count plus one
// times chunkSize, whatever the asset's size.
type splitBody struct {
	client       *Client
	ctx          context.Context
	cancel       context.CancelFunc
	url          string
	insecure     bool
	precondition header
	length       int64

	// head is the first chunk, still arriving on the response that started the
	// download, and headBody is that response.
	head       io.Reader
	headBody   io.ReadCloser
	headLength int64
	headRead   int64

	// chunks[index] hands one chunk from the worker that fetched it to the
	// reader. Entry zero is the head, which no worker fetches. The channels are
	// unbuffered on purpose: a worker that could drop a finished chunk and move
	// on would read the whole asset into memory ahead of the hash.
	chunks  []chan chunkResult
	taken   atomic.Int64
	index   int
	current *bytes.Reader
	failed  error

	release func()
	workers sync.WaitGroup
	once    sync.Once
}

type chunkResult struct {
	data []byte
	err  error
}

func (client *Client) split(ctx context.Context, input application.FetchRequest, response *http.Response, cancel, cancelHead context.CancelFunc, precondition header, chunks, workers int) io.ReadCloser {
	headLength := min(int64(chunkSize), response.ContentLength)
	guarded := guard(response.Body, cancelHead, client.options.Timeout)
	body := &splitBody{
		client:       client,
		ctx:          ctx,
		cancel:       cancel,
		url:          response.Request.URL.String(),
		insecure:     input.AllowInsecureHTTP,
		precondition: precondition,
		length:       response.ContentLength,
		head:         io.LimitReader(guarded, headLength),
		headBody:     guarded,
		headLength:   headLength,
		chunks:       make([]chan chunkResult, chunks),
		index:        1,
		release:      func() { client.release(workers) },
	}
	for index := 1; index < chunks; index++ {
		body.chunks[index] = make(chan chunkResult)
	}
	body.taken.Store(1)
	body.workers.Add(workers)
	for range workers {
		go body.work()
	}
	return body
}

// work fetches chunks until they run out. A worker blocks handing one over
// until the reader has caught up, which is what keeps a fast worker from
// running far ahead of the hash and buffering the whole asset.
//
// A worker that fails stops claiming chunks, so the last chunks can end up with
// nobody to fetch them. That cannot strand the reader: chunks are claimed in
// order and a worker always hands over the chunk it claimed, so every chunk
// before an unclaimed one carries a result, and the failure among them is what
// the reader stops at.
func (body *splitBody) work() {
	defer body.workers.Done()
	for {
		index := int(body.taken.Add(1) - 1)
		if index >= len(body.chunks) {
			return
		}
		data, err := body.fetch(index)
		select {
		case body.chunks[index] <- chunkResult{data: data, err: err}:
		case <-body.ctx.Done():
			return
		}
		if err != nil {
			// The reader will report this chunk and stop. Leaving the rest
			// unclaimed lets the other workers finish what they hold rather
			// than starting requests for bytes nobody will read.
			return
		}
	}
}

// Read delivers the asset in order. It keeps the error that ended the download,
// because the chunk that reported one has already been taken off its channel,
// and a reader that tries again would otherwise wait for bytes nobody is
// fetching.
func (body *splitBody) Read(buffer []byte) (int, error) {
	if body.failed != nil {
		return 0, body.failed
	}
	if body.headBody != nil {
		count, err := body.readHead(buffer)
		if err != nil {
			body.failed = err
		}
		// A head that is still open has more of the first chunk to deliver, even
		// when this read produced none of it. Only its handover falls through to
		// the chunks the range requests carry.
		if count > 0 || err != nil || body.headBody != nil {
			return count, err
		}
	}
	for body.current == nil || body.current.Len() == 0 {
		if body.current != nil {
			body.current = nil
			body.index++
		}
		if body.index >= len(body.chunks) {
			body.failed = io.EOF
			return 0, io.EOF
		}
		select {
		case result := <-body.chunks[body.index]:
			if result.err != nil {
				body.failed = result.err
				return 0, result.err
			}
			body.current = bytes.NewReader(result.data)
		case <-body.ctx.Done():
			body.failed = body.ctx.Err()
			return 0, body.failed
		}
	}
	return body.current.Read(buffer)
}

// readHead reads the chunk the first response carries. It reports the handover
// to the range requests as a read of no bytes and no error, because the end of
// that response is not the end of the asset.
func (body *splitBody) readHead(buffer []byte) (int, error) {
	count, err := body.head.Read(buffer)
	body.headRead += int64(count)
	if err == nil || !errors.Is(err, io.EOF) {
		return count, err
	}
	_ = body.headBody.Close()
	body.head, body.headBody = nil, nil
	if body.headRead < body.headLength {
		return count, io.ErrUnexpectedEOF
	}
	return count, nil
}

func (body *splitBody) Close() error {
	body.once.Do(func() {
		if body.headBody != nil {
			_ = body.headBody.Close()
			body.head, body.headBody = nil, nil
		}
		body.cancel()
		// The workers hold the parallelism this download borrowed, so it goes
		// back only once they have stopped using it.
		body.workers.Wait()
		body.release()
	})
	return nil
}

// fetch downloads one chunk, retrying it on its own.
//
// A whole-body transfer that fails halfway is lost, because its bytes have
// already gone to the hash. A chunk has not been handed over yet, so a retry
// here costs one chunk rather than the asset.
func (body *splitBody) fetch(index int) ([]byte, error) {
	start := int64(index) * chunkSize
	end := min(start+chunkSize, body.length) - 1
	data, _, err := retry(body.ctx, body.client.options.Retries, func() ([]byte, error) {
		return body.attempt(start, end)
	}, nil)
	if err != nil {
		return nil, &RequestError{URL: body.url, Status: statusOf(err), Err: err}
	}
	return data, nil
}

func (body *splitBody) attempt(start, end int64) ([]byte, error) {
	requestCtx, cancel := context.WithCancel(body.ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, body.url, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	request.Header.Set(body.precondition.name, body.precondition.value)
	httpClient := &http.Client{Transport: body.client.transport, CheckRedirect: body.client.checkRedirect(body.insecure)}
	// The body is closed by the guard below, which takes ownership of it and
	// closes it along with stopping its timer. bodyclose follows the response
	// only as far as the wrapper, so it cannot see that.
	response, err := httpClient.Do(request) //nolint:bodyclose // guard owns and closes it
	if err != nil {
		return nil, err
	}
	body.client.options.Logger.Debug("range", "url", body.url,
		"start", start, "end", end, "status", response.StatusCode)
	// A chunk gets the same stall guard as a whole transfer, and its own
	// cancellation with it, so one slow connection fails and is retried instead
	// of holding up the download behind it.
	guarded := guard(response.Body, cancel, body.client.options.Timeout)
	defer func() { _ = guarded.Close() }()
	if err := checkRange(response, start, end); err != nil {
		return nil, err
	}
	data := make([]byte, end-start+1)
	if _, err := io.ReadFull(guarded, data); err != nil {
		return nil, err
	}
	return data, nil
}

// checkRange refuses a response that is not the range that was asked for. The
// digest check would catch a misassembled asset in the end, but it could only
// report that the bytes were wrong, and the reason they were wrong is here.
func checkRange(response *http.Response, start, end int64) error {
	if response.StatusCode != http.StatusPartialContent {
		return &statusError{kind: "range", statusCode: response.StatusCode, retryAfter: retryAfter(response)}
	}
	encoding := strings.TrimSpace(response.Header.Get("Content-Encoding"))
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return fmt.Errorf("response content encoding %q is not identity", encoding)
	}
	first, last, err := contentRange(response.Header.Get("Content-Range"))
	if err != nil {
		return err
	}
	if first != start || last != end {
		return fmt.Errorf("server returned bytes %d-%d for the range %d-%d", first, last, start, end)
	}
	return nil
}

// contentRange returns the first and last byte position a Content-Range names.
func contentRange(value string) (int64, int64, error) {
	invalid := func() (int64, int64, error) {
		return 0, 0, fmt.Errorf("response content range %q is not a byte range", value)
	}
	unit, span, found := strings.Cut(strings.TrimSpace(value), " ")
	if !found || !strings.EqualFold(unit, "bytes") {
		return invalid()
	}
	span, _, found = strings.Cut(strings.TrimSpace(span), "/")
	if !found {
		return invalid()
	}
	firstValue, lastValue, found := strings.Cut(span, "-")
	if !found {
		return invalid()
	}
	first, firstErr := strconv.ParseInt(firstValue, 10, 64)
	last, lastErr := strconv.ParseInt(lastValue, 10, 64)
	if firstErr != nil || lastErr != nil {
		return invalid()
	}
	return first, last, nil
}

// acquire takes up to want range-request slots and reports how many it got.
//
// The slots are the whole client's, so the parallelism an operator asks for is
// a budget rather than a multiplier: one large asset splits every way it can,
// while a pull of many assets spends the same budget across the transfers it
// already runs side by side instead of opening the product of the two.
func (client *Client) acquire(want int) int {
	taken := 0
	for taken < want {
		select {
		case client.budget <- struct{}{}:
			taken++
		default:
			return taken
		}
	}
	return taken
}

func (client *Client) release(count int) {
	for range count {
		<-client.budget
	}
}
