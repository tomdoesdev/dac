// Package getit fetches files over HTTP.
//
// A Getter is built once and shared. It is safe for concurrent use, and every
// setting is fixed at New, so nothing a transfer does can change what another
// transfer will do:
//
//	getter := getit.New(getit.WithDefaults(getit.WithParts(4)))
//	defer getter.Close()
//
//	response, err := getter.Get(ctx, url)
//	if err != nil {
//		return err
//	}
//	defer func() { _ = response.Body.Close() }()
//	_, err = io.Copy(destination, response.Body)
//
// Get returns a stream rather than writing a file, so that a caller hashing,
// unpacking, or verifying the bytes does so as they arrive. Close the body of
// every response, including one whose NotModified is set.
//
// # Split downloads
//
// WithParts allows one file to be read over several requests at once. The first
// request is an ordinary GET, and its response answers three questions
// together: how large the file is, whether the origin serves byte ranges, and
// what validator ties the ranges to one set of bytes. Its body is the first
// chunk, so nothing about the probe is wasted and no HEAD is sent.
//
// The chunks after it are requested as byte ranges, each with a precondition
// naming what the first response described, so that a file that changes under a
// download fails rather than arriving mixed. Where any of that is missing the
// body streams over the first response instead, so a split is something this
// package does when it can, not something a caller can rely on.
//
// Bytes are delivered in order, which is what makes the result a stream. A
// chunk that arrives early waits in memory for its turn, so the memory one
// transfer costs is about one chunk per part.
//
// # Extending it
//
// Two seams take behavior that is not this package's to decide.
// WithTransportDecorator wraps every request, including each redirect and each
// range, which is where credentials, rate limits, and refusals belong.
// WithHooks reports each request and its bytes while they are in flight, which
// is where progress belongs.
package getit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Getter fetches files. It is safe for concurrent use.
type Getter struct {
	settings settings
	client   *http.Client
	// base is the transport this package built, or nil when the caller gave one.
	base *http.Transport
	// budget holds one slot per range request that this Getter may add to the
	// transfers it is already running. A nil budget is no ceiling.
	budget  chan struct{}
	counter atomic.Uint64
}

// Response is what one origin answered with.
type Response struct {
	// Status is the HTTP status: 200, or 304 for a conditional request whose
	// bytes have not changed.
	Status int
	// NotModified reports that the caller already holds these bytes. It is only
	// ever set for a request that carried a validator.
	NotModified bool
	// Validator is what the origin says these bytes are. Keep it to ask the same
	// question again later with WithValidator.
	Validator Validator
	// Length is the size of the body, or Unknown when the origin declared none.
	Length int64
	// URL is the URL that answered, which is the end of any redirect chain.
	URL *url.URL
	// Header is the response header, whole. What a file should be called is a
	// caller's decision, and Content-Disposition and URL are where it is made.
	Header http.Header
	// Body carries the bytes. Read it from one goroutine, as with the body of an
	// http.Response, and close it whatever else happens: a body left open holds
	// a connection, and the parts of a split download, until it is closed.
	Body io.ReadCloser
}

// New builds a Getter.
func New(options ...Option) *Getter {
	current := newSettings(options)
	getter := &Getter{settings: current}
	transport := current.transport
	if transport == nil {
		getter.base = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: current.connectTimeout, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          max(32, current.budget*2),
			MaxIdleConnsPerHost:   max(8, current.budget),
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   current.connectTimeout,
			ResponseHeaderTimeout: current.connectTimeout,
			// Compression is off for the same reason Accept-Encoding is identity:
			// a compressed body would make the length and the byte offsets
			// describe something other than the bytes that reach the caller.
			DisableCompression: true,
		}
		transport = getter.base
	}
	// The first decorator is the outermost, so it is the first to see a request.
	for index := len(current.decorators) - 1; index >= 0; index-- {
		transport = current.decorators[index](transport)
	}
	getter.client = &http.Client{Transport: transport, CheckRedirect: getter.checkRedirect}
	if current.budget > 0 {
		getter.budget = make(chan struct{}, current.budget)
	}
	return getter
}

// Close releases the connections this Getter is holding idle.
// It does not stop the transfers that are running. Cancel their contexts, or
// close their bodies, to do that.
func (getter *Getter) Close() {
	if getter.base != nil {
		getter.base.CloseIdleConnections()
		return
	}
	if closer, ok := getter.settings.transport.(interface{ CloseIdleConnections() }); ok {
		closer.CloseIdleConnections()
	}
}

// Get fetches one file and returns its body for reading.
//
// The body must be closed, whatever the caller does with it. A failure is a
// *RequestError naming the URL, and the status when the request got one.
func (getter *Getter) Get(ctx context.Context, rawURL string, options ...RequestOption) (*Response, error) {
	return getter.request(ctx, http.MethodGet, rawURL, options...)
}

// Head inspects one file without transferring its body.
// It applies the same URL policy, redirects, retries, validators, decorators,
// and response checks as Get. The caller must close the empty response body.
func (getter *Getter) Head(ctx context.Context, rawURL string, options ...RequestOption) (*Response, error) {
	return getter.request(ctx, http.MethodHead, rawURL, options...)
}

// request performs one whole-file GET or HEAD through the shared request policy.
func (getter *Getter) request(ctx context.Context, method, rawURL string, options ...RequestOption) (*Response, error) {
	current := newRequestSettings(getter.settings.request, options)
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, &RequestError{URL: rawURL, Err: Permanent(err)}
	}
	if err := getter.settings.urlPolicy(target); err != nil {
		return nil, &RequestError{URL: rawURL, Err: Permanent(err)}
	}
	identifier := getter.counter.Add(1)
	trace := getter.settings.logger
	message := "fetching"
	if method == http.MethodHead {
		message = "probing"
	}
	trace.Debug(message, "method", method, "url", target, "conditional", !current.validator.Empty())
	result, err := retry(ctx, current.retries, getter.settings.retryPolicy,
		func(attempt int) (attemptResult, error) {
			return getter.attempt(ctx, method, target, current, identifier, attempt)
		},
		func(attempt int, err error, wait time.Duration) {
			current.hooks.retry(wholeRequest(identifier, method, target, attempt), err, wait)
			trace.Debug("retrying", "method", method, "url", target, "attempt", attempt,
				"after", wait, "status", statusOf(err), "error", err)
		})
	if err != nil {
		trace.Debug("giving up", "method", method, "url", target, "status", statusOf(err),
			"retryable", getter.settings.retryPolicy(err), "error", err)
		return nil, &RequestError{URL: target.String(), Status: statusOf(err), Err: err}
	}
	trace.Debug("response", "method", method, "url", target, "status", result.response.Status,
		"notModified", result.response.NotModified, "length", result.response.Length,
		"parts", result.transfer.Parts)
	current.hooks.start(result.transfer)
	return result.response, nil
}

// attemptResult is one successful attempt: what to give the caller, and what to
// report the transfer as. The two are separate because Parts is decided here,
// after the first response has said whether this download can be split.
type attemptResult struct {
	response *Response
	transfer Transfer
}

func (getter *Getter) attempt(ctx context.Context, method string, target *url.URL, current requestSettings, identifier uint64, attempt int) (attemptResult, error) {
	info := wholeRequest(identifier, method, target, attempt)
	downloadCtx, cancelDownload := context.WithCancel(ctx)
	// A child context lets a split download close the first response once its
	// chunk runs out, without stopping the ranges beside it.
	headCtx, cancelHead := context.WithCancel(downloadCtx)
	cancel := func() { cancelHead(); cancelDownload() }
	request, err := http.NewRequestWithContext(headCtx, method, target.String(), nil)
	if err != nil {
		cancel()
		return attemptResult{}, err
	}
	applyHeader(request, current.header)
	request.Header.Set("Accept-Encoding", "identity")
	current.validator.revalidate(request.Header)

	current.hooks.request(info)
	response, err := getter.client.Do(request) //nolint:bodyclose // the guard or the failure path owns it
	if err != nil {
		current.hooks.response(info, 0, err)
		cancel()
		return attemptResult{}, err
	}
	current.hooks.response(info, response.StatusCode, nil)
	if err := checkResponse(response, !current.validator.Empty()); err != nil {
		_ = response.Body.Close()
		cancel()
		return attemptResult{}, err
	}
	if current.maxBytes > 0 && response.ContentLength > current.maxBytes {
		_ = response.Body.Close()
		cancel()
		return attemptResult{}, Permanent(fmt.Errorf("%w of %d bytes: the origin declared %d",
			ErrTooLarge, current.maxBytes, response.ContentLength))
	}

	final := target
	if response.Request != nil && response.Request.URL != nil {
		final = response.Request.URL
	}
	validator := validatorFrom(response.Header)
	if response.StatusCode == http.StatusNotModified {
		// Nothing is transferred, so there is nothing to guard, count, or split.
		transfer := Transfer{ID: identifier, URL: target, Length: 0, Parts: 0}
		return attemptResult{
			response: &Response{
				Status:      response.StatusCode,
				NotModified: true,
				Validator:   validator,
				Length:      0,
				URL:         final,
				Header:      response.Header,
				Body:        newTransferBody(&plainBody{body: response.Body, cancel: cancel}, transfer, current.hooks),
			},
			transfer: transfer,
		}, nil
	}
	if method == http.MethodHead {
		transfer := Transfer{ID: identifier, URL: target, Length: 0, Parts: 0}
		return attemptResult{
			response: &Response{
				Status:    response.StatusCode,
				Validator: validator,
				Length:    response.ContentLength,
				URL:       final,
				Header:    response.Header,
				Body:      newTransferBody(&plainBody{body: response.Body, cancel: cancel}, transfer, current.hooks),
			},
			transfer: transfer,
		}, nil
	}

	// ContentLength is already -1 when the origin declared no length, which is
	// what Unknown is.
	state := &exchange{
		getter:   getter,
		settings: current,
		transfer: identifier,
		url:      final,
		length:   response.ContentLength,
		received: &atomic.Int64{},
	}
	inner, parts := getter.body(downloadCtx, state, response, cancel, cancelHead)
	transfer := Transfer{ID: identifier, URL: target, Length: response.ContentLength, Parts: parts}
	return attemptResult{
		response: &Response{
			Status:    response.StatusCode,
			Validator: validator,
			Length:    response.ContentLength,
			URL:       final,
			Header:    response.Header,
			Body:      newTransferBody(inner, transfer, current.hooks),
		},
		transfer: transfer,
	}, nil
}

// wholeRequest describes a request for the whole body, for the reports about it.
func wholeRequest(identifier uint64, method string, target *url.URL, attempt int) Request {
	return Request{
		Transfer: identifier,
		Part:     0,
		Attempt:  attempt,
		Method:   method,
		URL:      target,
		Start:    Unknown,
		End:      Unknown,
	}
}

// checkRedirect applies the URL policy to each redirect target.
// A redirect is where a policy that only looked at the URL a caller gave would
// be got around, so the policy is applied to every hop instead.
func (getter *Getter) checkRedirect(request *http.Request, via []*http.Request) error {
	if len(via) > getter.settings.maxRedirects {
		return Permanent(fmt.Errorf("server returned more than %d redirects", getter.settings.maxRedirects))
	}
	if err := getter.settings.urlPolicy(request.URL); err != nil {
		return Permanent(err)
	}
	getter.settings.logger.Debug("redirect", "to", request.URL.String(), "hops", len(via))
	return nil
}

func checkResponse(response *http.Response, conditional bool) error {
	if err := checkEncoding(response); err != nil {
		return err
	}
	if response.StatusCode == http.StatusOK {
		return nil
	}
	if conditional && response.StatusCode == http.StatusNotModified {
		return nil
	}
	kind := "request"
	if conditional {
		kind = "conditional request"
	}
	return &StatusError{Kind: kind, Status: response.StatusCode, RetryAfter: retryAfter(response)}
}

// checkEncoding refuses a body that is not the bytes that were asked for.
// Every request goes out with Accept-Encoding: identity, so a content encoding
// here is an origin that ignored it. It matters more than it looks: with a
// coding in play, the length and the byte offsets stop describing the bytes the
// caller receives, and every range of a split download lands in the wrong place.
func checkEncoding(response *http.Response) error {
	encoding := strings.TrimSpace(response.Header.Get("Content-Encoding"))
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return nil
	}
	return Permanent(fmt.Errorf("response content encoding %q is not identity", encoding))
}

// applyHeader adds the caller's headers. The headers this package sets are set
// after these, so its own values replace anything named twice.
func applyHeader(request *http.Request, header http.Header) {
	for name, values := range header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
}

// transferBody is the body a caller reads, and the one place that reports the
// end of a transfer.
type transferBody struct {
	inner    io.ReadCloser
	transfer Transfer
	hooks    hookList

	// mutex guards failed, because a caller is allowed to close a body from
	// another goroutine to stop a read that is under way.
	mutex  sync.Mutex
	failed error
	once   sync.Once
}

func newTransferBody(inner io.ReadCloser, transfer Transfer, hooks hookList) io.ReadCloser {
	return &transferBody{inner: inner, transfer: transfer, hooks: hooks}
}

func (body *transferBody) Read(buffer []byte) (int, error) {
	count, err := body.inner.Read(buffer)
	// The end of the body is passed back exactly as it arrived, because io.Copy
	// compares it against io.EOF rather than unwrapping it.
	if err != nil && !errors.Is(err, io.EOF) {
		body.mutex.Lock()
		if body.failed == nil {
			body.failed = err
		}
		body.mutex.Unlock()
	}
	return count, err
}

func (body *transferBody) Close() error {
	err := body.inner.Close()
	body.once.Do(func() {
		body.mutex.Lock()
		failure := body.failed
		body.mutex.Unlock()
		if failure == nil {
			failure = err
		}
		body.hooks.end(body.transfer, failure)
	})
	return err
}

// plainBody closes a response and the request behind it, with none of the
// guarding that a body carrying bytes needs.
type plainBody struct {
	body   io.ReadCloser
	cancel context.CancelFunc
}

func (body *plainBody) Read(buffer []byte) (int, error) { return body.body.Read(buffer) }

func (body *plainBody) Close() error {
	err := body.body.Close()
	body.cancel()
	return err
}
