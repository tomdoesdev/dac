// Package httpclient fetches remote asset bytes.
package httpclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/debug"
	"github.com/tomdoesdev/dac/internal/filename"
	"github.com/tomdoesdev/dac/internal/urlpolicy"
)

const maxRedirects = 10

var _ application.RequestDetail = (*RequestError)(nil)

// Options configures one HTTP client.
type Options struct {
	Timeout time.Duration
	Retries int
	// Parallelism is how many requests one asset may be split across when the origin serves byte ranges.
	Parallelism int
	// TransportDecorators add optional request behavior around the base HTTP transport.
	TransportDecorators []TransportDecorator
	// Logger traces what each transfer did. A nil logger traces nothing.
	Logger *slog.Logger
}

// TransportDecorator adds request behavior without changing the transfer engine.
type TransportDecorator func(http.RoundTripper) http.RoundTripper

// Client owns the connections used for asset requests.
type Client struct {
	options   Options
	transport http.RoundTripper
	base      *http.Transport
	// budget holds one slot per range request the client may add to the transfers it is already running.
	budget chan struct{}
}

// New creates an HTTP asset client.
func New(options Options) *Client {
	options.Parallelism = max(options.Parallelism, 1)
	options.Logger = debug.Or(options.Logger)
	base := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: options.Timeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          max(32, options.Parallelism*4),
		MaxIdleConnsPerHost:   max(8, options.Parallelism*2),
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   options.Timeout,
		ResponseHeaderTimeout: options.Timeout,
		DisableCompression:    true,
	}
	var transport http.RoundTripper = base
	for index := len(options.TransportDecorators) - 1; index >= 0; index-- {
		transport = options.TransportDecorators[index](transport)
	}
	return &Client{
		options:   options,
		transport: transport,
		base:      base,
		budget:    make(chan struct{}, options.Parallelism),
	}
}

// Close releases idle connections.
func (client *Client) Close() { client.base.CloseIdleConnections() }

// Fetch sends an unconditional or conditional asset request.
func (client *Client) Fetch(ctx context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
	parsed, err := urlpolicy.ParseAndCheck(request.URL, request.AllowInsecureHTTP)
	if err != nil {
		return nil, &RequestError{URL: request.URL, Err: err}
	}
	request.URL = parsed.String()
	trace := client.options.Logger
	trace.Debug("fetching", "url", request.URL, "conditional", request.ETag != "")
	response, attempts, err := retry(ctx, client.options.Retries, func() (*application.FetchResponse, error) {
		return client.attempt(ctx, request)
	}, func(attempt int, err error, wait time.Duration) {
		trace.Debug("retrying", "url", request.URL, "attempt", attempt,
			"after", wait, "status", statusOf(err), "error", err)
	})
	if err != nil {
		trace.Debug("giving up", "url", request.URL, "attempts", attempts,
			"status", statusOf(err), "retryable", retryable(err), "error", err)
		return nil, &RequestError{URL: request.URL, Status: statusOf(err), Err: err}
	}
	trace.Debug("response", "url", request.URL, "attempt", attempts-1,
		"notModified", response.NotModified, "length", response.Length,
		"etag", response.ETag, "filename", response.Filename)
	return response, nil
}

// RequestError names the request behind a transport failure.
// The boundary adds request details after retry logic inspects transport errors.
type RequestError struct {
	URL string
	// Status is the HTTP status, or zero when the request never got a response.
	Status int
	Err    error
}

func (value *RequestError) Error() string { return fmt.Sprintf("%s: %v", value.URL, value.Err) }

func (value *RequestError) Unwrap() error { return value.Err }

// RequestURL and StatusCode satisfy application.RequestDetail.
func (value *RequestError) RequestURL() string { return value.URL }

func (value *RequestError) StatusCode() int { return value.Status }

func statusOf(err error) int {
	var status *statusError
	if errors.As(err, &status) {
		return status.statusCode
	}
	return 0
}

func (client *Client) attempt(ctx context.Context, input application.FetchRequest) (*application.FetchResponse, error) {
	downloadCtx, cancelDownload := context.WithCancel(ctx)
	// A child context lets split mode close the first response without stopping ranges.
	headCtx, cancelHead := context.WithCancel(downloadCtx)
	cancel := func() { cancelHead(); cancelDownload() }
	request, err := http.NewRequestWithContext(headCtx, http.MethodGet, input.URL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	request.Header.Set("Accept-Encoding", "identity")
	if input.ETag != "" {
		request.Header.Set("If-None-Match", input.ETag)
	}
	httpClient := &http.Client{Transport: client.transport, CheckRedirect: client.checkRedirect(input.AllowInsecureHTTP)}
	response, err := httpClient.Do(request)
	if err != nil {
		cancel()
		return nil, err
	}
	if err := checkResponse(response, input.ETag); err != nil {
		_ = response.Body.Close()
		cancel()
		return nil, err
	}
	return &application.FetchResponse{
		NotModified: response.StatusCode == http.StatusNotModified,
		ETag:        response.Header.Get("ETag"),
		Filename:    responseFilename(response),
		Length:      response.ContentLength,
		Body:        client.body(downloadCtx, input, response, cancel, cancelHead),
	}, nil
}

// responseFilename reports the name the origin gives an asset.
// A Content-Disposition header is the origin saying the name outright, so it wins.
func responseFilename(response *http.Response) string {
	if name := filename.FromDisposition(response.Header.Get("Content-Disposition")); name != "" {
		return name
	}
	// Request is the final request of the redirect chain, and its URL is resolved against the one before it.
	if response.Request == nil || response.Request.URL == nil {
		return ""
	}
	return filename.FromURL(response.Request.URL.String())
}

// checkRedirect applies the URL policy to each redirect target.
func (client *Client) checkRedirect(allowInsecure bool) func(*http.Request, []*http.Request) error {
	return func(request *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects {
			return fmt.Errorf("server returned more than %d redirects", maxRedirects)
		}
		if err := urlpolicy.Check(request.URL, allowInsecure); err != nil {
			return err
		}
		client.options.Logger.Debug("redirect", "to", request.URL.String(), "hops", len(via))
		return nil
	}
}

func checkResponse(response *http.Response, etag string) error {
	encoding := strings.TrimSpace(response.Header.Get("Content-Encoding"))
	if encoding != "" && !strings.EqualFold(encoding, "identity") {
		return fmt.Errorf("response content encoding %q is not identity", encoding)
	}
	if response.StatusCode == http.StatusOK || etag != "" && response.StatusCode == http.StatusNotModified {
		return nil
	}
	kind := "unconditional"
	if etag != "" {
		kind = "conditional"
	}
	return &statusError{kind: kind, statusCode: response.StatusCode, retryAfter: retryAfter(response)}
}

type statusError struct {
	kind       string
	statusCode int
	retryAfter time.Duration
}

func (value *statusError) Error() string {
	return fmt.Sprintf("%s request returned HTTP %d", value.kind, value.statusCode)
}

func retryAfter(response *http.Response) time.Duration {
	value := strings.TrimSpace(response.Header.Get("Retry-After"))
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Until(when), 0)
	}
	return 0
}

func retryable(err error) bool {
	var status *statusError
	if errors.As(err, &status) {
		return status.statusCode == http.StatusTooManyRequests ||
			status.statusCode == http.StatusRequestTimeout ||
			status.statusCode >= 500 && status.statusCode <= 599
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return !errors.Is(err, urlpolicy.ErrNotPermitted)
}

// retry applies one retry policy to full requests and range requests.
func retry[T any](ctx context.Context, retries int, operation func() (T, error), notify func(int, error, time.Duration)) (T, int, error) {
	for attempt := 0; ; attempt++ {
		value, err := operation()
		if err == nil {
			return value, attempt + 1, nil
		}
		if attempt >= retries || !retryable(err) || ctx.Err() != nil {
			var zero T
			return zero, attempt + 1, err
		}
		wait := backoff(attempt, err)
		if notify != nil {
			notify(attempt+1, err, wait)
		}
		if err := sleep(ctx, wait); err != nil {
			var zero T
			return zero, attempt + 1, err
		}
	}
}

func backoff(attempt int, err error) time.Duration {
	var status *statusError
	if errors.As(err, &status) && status.retryAfter > 0 {
		return min(status.retryAfter, 30*time.Second)
	}
	wait := min(time.Duration(1<<attempt)*250*time.Millisecond, 8*time.Second)
	return wait + rand.N(wait/2+time.Millisecond)
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
