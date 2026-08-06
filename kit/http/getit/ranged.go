package getit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// exchange is what every request of one transfer needs to know about it.
type exchange struct {
	getter   *Getter
	settings requestSettings
	// transfer is the ID that reports name this transfer by.
	transfer uint64
	// url is the URL of the first response, which is the end of any redirect
	// chain. Ranges ask that URL rather than the one the caller gave, so that a
	// redirect is followed once instead of once per range.
	url *url.URL
	// length is the size of the whole body.
	length int64
	// received is the running byte count that every part adds to.
	received *atomic.Int64
}

// body returns the reader that delivers one response's bytes, and how many
// requests will carry them.
func (getter *Getter) body(ctx context.Context, state *exchange, response *http.Response, cancel, cancelHead context.CancelFunc) (io.ReadCloser, int) {
	trace := getter.settings.logger
	validator, refused := getter.splittable(state.settings, response)
	if refused != "" {
		trace.Debug("streaming one response", "url", state.url, "reason", refused, "length", response.ContentLength)
		return guard(response.Body, cancel, 0, state), 1
	}
	// The first response already carries the first chunk, so only the chunks
	// after it need a worker.
	chunkSize := getter.settings.chunkSize
	chunks := int((response.ContentLength + chunkSize - 1) / chunkSize)
	workers := getter.acquire(min(state.settings.parts-1, chunks-1))
	if workers == 0 {
		trace.Debug("streaming one response", "url", state.url, "reason", "no parts to spare", "length", response.ContentLength)
		return guard(response.Body, cancel, 0, state), 1
	}
	trace.Debug("splitting download", "url", state.url, "length", response.ContentLength,
		"chunks", chunks, "workers", workers)
	return getter.split(ctx, state, response, cancel, cancelHead, validator, chunks, workers), chunks
}

// splittable returns the validator that holds a split download together, or the
// reason this response cannot be split. The reason is what a trace needs.
//
// A split download reads one file over several requests, so it has to be sure
// that every request is answered about the same bytes.
func (getter *Getter) splittable(current requestSettings, response *http.Response) (Validator, string) {
	chunkSize := getter.settings.chunkSize
	switch {
	case !current.split:
		return Validator{}, "splitting is off"
	case current.parts < 2:
		return Validator{}, "parts is 1"
	case response.StatusCode != http.StatusOK:
		return Validator{}, "response is not 200"
	case response.ContentLength < 0:
		return Validator{}, "response has no length"
	// A file below two chunks has no second chunk to give anybody.
	case response.ContentLength < 2*chunkSize:
		return Validator{}, "response is below the split size"
	}
	if !acceptsRanges(response.Header.Get("Accept-Ranges")) {
		return Validator{}, "origin does not serve byte ranges"
	}
	validator := validatorFrom(response.Header)
	if _, _, ok := validator.precondition(); !ok {
		return Validator{}, "origin sent no strong validator"
	}
	return validator, ""
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
type splitBody struct {
	state     *exchange
	ctx       context.Context
	cancel    context.CancelFunc
	validator Validator

	// head is the first chunk, still arriving on the response that started the
	// download, and headBody is that response.
	head       io.Reader
	headBody   io.ReadCloser
	headLength int64
	headRead   int64

	// chunks[index] hands one chunk from the worker that fetched it to the reader.
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

func (getter *Getter) split(ctx context.Context, state *exchange, response *http.Response, cancel, cancelHead context.CancelFunc, validator Validator, chunks, workers int) io.ReadCloser {
	headLength := min(getter.settings.chunkSize, response.ContentLength)
	// The first response gets a child context, so that closing it when its chunk
	// runs out does not stop the ranges beside it.
	guarded := guard(response.Body, cancelHead, 0, state)
	body := &splitBody{
		state:      state,
		ctx:        ctx,
		cancel:     cancel,
		validator:  validator,
		head:       io.LimitReader(guarded, headLength),
		headBody:   guarded,
		headLength: headLength,
		chunks:     make([]chan chunkResult, chunks),
		index:      1,
		release:    func() { getter.release(workers) },
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

// work fetches chunks until they run out.
// A worker that fails stops claiming chunks, so the chunks after it can end up
// with nobody to fetch them. That is correct, because the reader reports the
// first chunk that failed and stops there.
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
			return
		}
	}
}

// Read delivers the file in order.
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
		// when this read produced none of it.
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

// readHead reads the chunk that the first response carries.
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
		// The workers hold the parts this download borrowed, so those go back
		// only once the workers have stopped using them.
		body.workers.Wait()
		body.release()
	})
	return nil
}

// fetch downloads one chunk, retrying it on its own.
// A whole body that fails halfway is lost, because the caller has already read
// the bytes before the failure and cannot be given them again. One chunk that
// fails has been read by nobody yet.
func (body *splitBody) fetch(index int) ([]byte, error) {
	chunkSize := body.state.getter.settings.chunkSize
	start := int64(index) * chunkSize
	end := min(start+chunkSize, body.state.length) - 1
	data, err := retry(body.ctx, body.state.settings.retries, body.state.getter.settings.retryPolicy,
		func(attempt int) ([]byte, error) { return body.attempt(index, start, end, attempt) },
		func(attempt int, err error, wait time.Duration) {
			body.state.settings.hooks.retry(body.state.request(index, attempt, start, end), err, wait)
		})
	if err != nil {
		return nil, &RequestError{URL: body.state.url.String(), Status: statusOf(err), Err: err}
	}
	return data, nil
}

// request describes one range request for the reports about it.
func (state *exchange) request(part, attempt int, start, end int64) Request {
	return Request{
		Transfer: state.transfer,
		Part:     part,
		Attempt:  attempt,
		Method:   http.MethodGet,
		URL:      state.url,
		Start:    start,
		End:      end,
	}
}

func (body *splitBody) attempt(index int, start, end int64, attempt int) ([]byte, error) {
	state := body.state
	info := state.request(index, attempt, start, end)
	requestCtx, cancel := context.WithCancel(body.ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, state.url.String(), nil)
	if err != nil {
		return nil, err
	}
	applyHeader(request, state.settings.header)
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	if name, value, ok := body.validator.precondition(); ok {
		request.Header.Set(name, value)
	}
	state.settings.hooks.request(info)
	// The guard below takes ownership of the body and closes it.
	response, err := state.getter.client.Do(request) //nolint:bodyclose // guard owns and closes it
	if err != nil {
		state.settings.hooks.response(info, 0, err)
		return nil, err
	}
	state.settings.hooks.response(info, response.StatusCode, nil)
	state.getter.settings.logger.Debug("range", "url", state.url, "start", start, "end", end, "status", response.StatusCode)
	// Each range gets its own stall guard, so one slow connection can fail and
	// be retried without the rest of the download waiting on it.
	guarded := guard(response.Body, cancel, index, state)
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

// checkRange refuses a response that is not the range that was asked for.
func checkRange(response *http.Response, start, end int64) error {
	if response.StatusCode != http.StatusPartialContent {
		return &StatusError{Kind: "range request", Status: response.StatusCode, RetryAfter: retryAfter(response)}
	}
	if err := checkEncoding(response); err != nil {
		return err
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

// contentRange returns the first and last byte position that a Content-Range names.
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
func (getter *Getter) acquire(want int) int {
	if want <= 0 {
		return 0
	}
	if getter.budget == nil {
		return want
	}
	taken := 0
	for taken < want {
		select {
		case getter.budget <- struct{}{}:
			taken++
		default:
			return taken
		}
	}
	return taken
}

func (getter *Getter) release(count int) {
	if getter.budget == nil {
		return
	}
	for range count {
		<-getter.budget
	}
}
