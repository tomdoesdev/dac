// Package httpclient fetches remote asset bytes.
package httpclient

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/credential"
	"github.com/tom/dac/internal/rewrite"
	"github.com/tom/dac/internal/urlpolicy"
)

const maxRedirects = 10

var _ application.RequestDetail = (*RequestError)(nil)

// Options configures one HTTP client.
type Options struct {
	Timeout time.Duration
	Retries int
	// Parallelism is how many requests one asset may be split across when the
	// origin serves byte ranges. It is a budget for the whole client rather than
	// a per-download setting: see acquire in ranged.go. One disables splitting.
	Parallelism int
	// Rewriter decides the URL DAC requests for a canonical asset URL.
	Rewriter *rewrite.Config
	// Credentials supplies request headers for hosts that need them.
	Credentials *credential.Resolver
}

// Client owns the connections used for asset requests.
type Client struct {
	options   Options
	transport *http.Transport
	// budget holds one slot per range request the client may add to the
	// transfers it is already running.
	budget chan struct{}
}

// New creates an HTTP asset client.
func New(options Options) *Client {
	options.Parallelism = max(options.Parallelism, 1)
	return &Client{
		options: options,
		budget:  make(chan struct{}, options.Parallelism),
		transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: options.Timeout, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          max(32, options.Parallelism*4),
			MaxIdleConnsPerHost:   max(8, options.Parallelism*2),
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   options.Timeout,
			ResponseHeaderTimeout: options.Timeout,
			DisableCompression:    true,
		},
	}
}

// Close releases idle connections.
func (client *Client) Close() { client.transport.CloseIdleConnections() }

// Fetch sends an unconditional or conditional asset request.
//
// A rewrite config is applied before the URL policy, so a rewritten target is
// checked on its own terms rather than on the canonical URL's. The rewrite
// never reaches the lock file: callers keep passing the canonical URL, and only
// the request goes elsewhere.
func (client *Client) Fetch(ctx context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
	target, err := client.options.Rewriter.Apply(request.URL)
	if err != nil {
		return nil, &RequestError{URL: request.URL, Err: err}
	}
	if target.Rewritten && target.AllowInsecureHTTP {
		request.AllowInsecureHTTP = true
	}
	parsed, err := urlpolicy.ParseAndCheck(target.URL, request.AllowInsecureHTTP)
	if err != nil {
		return nil, &RequestError{URL: target.URL, Err: err}
	}
	request.URL = parsed.String()
	var lastErr error
	for attempt := 0; ; attempt++ {
		response, err := client.attempt(ctx, request)
		if err == nil {
			return response, nil
		}
		lastErr = err
		if attempt >= client.options.Retries || !retryable(err) || ctx.Err() != nil {
			return nil, &RequestError{URL: request.URL, Status: statusOf(lastErr), Err: lastErr}
		}
		if err := sleep(ctx, backoff(attempt, err)); err != nil {
			return nil, &RequestError{URL: request.URL, Err: err}
		}
	}
}

// RequestError names the request behind a transport failure.
//
// It wraps only at the boundary, so retry and backoff keep matching on the
// error the transport actually returned, while a caller reporting the failure
// can still say which URL it was and what the server said.
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
	// The first request gets a cancellation of its own inside the download's, so
	// that a split download can close it once it has delivered the first chunk
	// without cancelling the range requests that finish the asset. Cancelling
	// the download ends both.
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
	credentials := client.authorizer()
	if err := credentials.apply(headCtx, request); err != nil {
		cancel()
		return nil, err
	}

	httpClient := &http.Client{Transport: client.transport, CheckRedirect: client.checkRedirect(headCtx, input.AllowInsecureHTTP, credentials)}
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
		Length:      response.ContentLength,
		Body:        client.body(downloadCtx, input, response, cancel, cancelHead),
	}, nil
}

// checkRedirect applies URL policy, host policy, and credential rules to each
// redirect target.
func (client *Client) checkRedirect(ctx context.Context, allowInsecure bool, credentials *authorizer) func(*http.Request, []*http.Request) error {
	return func(request *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects {
			return fmt.Errorf("server returned more than %d redirects", maxRedirects)
		}
		if err := urlpolicy.Check(request.URL, allowInsecure); err != nil {
			return err
		}
		if err := client.options.Rewriter.Check(request.URL); err != nil {
			return err
		}
		return credentials.apply(ctx, request)
	}
}

// authorizer sets the credentials for each host in one redirect chain.
//
// It remembers every header name it has applied, because clearing Authorization
// alone does not clear credentials. A helper answers with whatever header its
// registry wants -- PRIVATE-TOKEN, X-JFrog-Art-Api, X-Api-Key -- and net/http
// copies the initial request's headers onto every redirect target, stripping
// only the four it knows are sensitive. Anything else would arrive at the next
// host still carrying the first host's secret.
//
// One authorizer belongs to one request chain, and net/http runs a chain's
// redirect callbacks on the goroutine that started it, so it needs no locking.
type authorizer struct {
	client *Client
	// applied names every header this chain has set. It accumulates rather than
	// replacing, because the headers copied onto a redirect come from the
	// initial request rather than from the hop before it.
	applied map[string]struct{}
}

func (client *Client) authorizer() *authorizer {
	return &authorizer{client: client, applied: map[string]struct{}{}}
}

// apply clears the credentials of the previous host and sets this host's.
func (credentials *authorizer) apply(ctx context.Context, request *http.Request) error {
	request.Header.Del("Authorization")
	for name := range credentials.applied {
		request.Header.Del(name)
	}
	header, err := credentials.client.options.Credentials.Headers(ctx, request.URL.String())
	if err != nil {
		return err
	}
	for name, values := range header {
		request.Header.Del(name)
		for _, value := range values {
			request.Header.Add(name, value)
		}
		credentials.applied[name] = struct{}{}
	}
	return nil
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
