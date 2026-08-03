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
	"sync"
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
	// One cache for the whole transfer, including the range requests that finish
	// a split download and every retry along the way.
	credentials := newCredentialCache(client)
	var lastErr error
	reauthorized := false
	for attempt := 0; ; {
		response, err := client.attempt(ctx, request, credentials)
		if err == nil {
			return response, nil
		}
		lastErr = err
		if !reauthorized && rejectedCredentials(err) && credentials.reset() {
			reauthorized = true
			continue
		}
		if attempt >= client.options.Retries || !retryable(err) || ctx.Err() != nil {
			return nil, &RequestError{URL: request.URL, Status: statusOf(lastErr), Err: lastErr}
		}
		if err := sleep(ctx, backoff(attempt, err)); err != nil {
			return nil, &RequestError{URL: request.URL, Err: err}
		}
		attempt++
	}
}

// rejectedCredentials reports a status that says the credentials were the
// problem. It is the one failure worth asking a helper again for: a cached
// answer can go stale mid-transfer, and a rejection is the only evidence of it
// DAC ever sees.
func rejectedCredentials(err error) bool {
	var status *statusError
	return errors.As(err, &status) &&
		(status.statusCode == http.StatusUnauthorized || status.statusCode == http.StatusForbidden)
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

func (client *Client) attempt(ctx context.Context, input application.FetchRequest, cache *credentialCache) (*application.FetchResponse, error) {
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
	credentials := cache.authorizer()
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
		Body:        client.body(downloadCtx, input, response, cancel, cancelHead, cache),
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

// credentialCache holds the headers a helper supplied for each URL in one
// transfer.
//
// A helper is a program DAC starts, and a split download asks for the same URL
// once per chunk. At an 8MiB chunk a 10GiB asset is over a thousand helper runs
// for a transfer the whole-body path pays for once, which is slow against a
// helper that shells out to a credential service and fatal against one that is
// rate limited: the download starts failing partway through for a reason
// nothing in the transfer explains.
//
// The key is the whole URL rather than the host. The helper protocol hands a
// helper the request URI, so a helper is entitled to answer differently for
// different paths, and a host key would quietly take that away. Every chunk of
// one download asks about one URL, so the narrower key costs nothing here.
//
// The cache belongs to one Fetch. A Client outlives every asset in a command,
// and a helper written to prompt or to audit each request should not find one
// asset answered out of what another collected.
type credentialCache struct {
	client  *Client
	mutex   sync.Mutex
	entries map[string]http.Header
}

func newCredentialCache(client *Client) *credentialCache {
	return &credentialCache{client: client, entries: map[string]http.Header{}}
}

// headers returns the credentials for one URL, asking the helper only for a URL
// this transfer has not seen. The result is shared, so a caller may read it but
// must not modify it.
//
// The helper runs with no lock held. Several workers starting on a cold cache
// can therefore run it at once, which costs one run per worker rather than the
// one a lock would buy -- and buys back the alternative, where every URL in the
// transfer waits behind whichever helper is slowest to answer.
func (cache *credentialCache) headers(ctx context.Context, url string) (http.Header, error) {
	cache.mutex.Lock()
	header, found := cache.entries[url]
	cache.mutex.Unlock()
	if found {
		return header, nil
	}
	header, err := cache.client.options.Credentials.Headers(ctx, url)
	if err != nil {
		return nil, err
	}
	cache.mutex.Lock()
	cache.entries[url] = header
	cache.mutex.Unlock()
	return header, nil
}

// reset forgets every answer and reports whether it had any. It clears the
// whole transfer rather than one URL because a rejection can come from a
// redirect target the caller never named, and re-asking about a URL that was
// fine costs one helper run.
func (cache *credentialCache) reset() bool {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if len(cache.entries) == 0 {
		return false
	}
	cache.entries = map[string]http.Header{}
	return true
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
// The cache behind it is shared by every chain in the transfer and does.
type authorizer struct {
	cache *credentialCache
	// applied names every header this chain has set. It accumulates rather than
	// replacing, because the headers copied onto a redirect come from the
	// initial request rather than from the hop before it.
	applied map[string]struct{}
}

func (cache *credentialCache) authorizer() *authorizer {
	return &authorizer{cache: cache, applied: map[string]struct{}{}}
}

// apply clears the credentials of the previous host and sets this host's.
func (credentials *authorizer) apply(ctx context.Context, request *http.Request) error {
	request.Header.Del("Authorization")
	for name := range credentials.applied {
		request.Header.Del(name)
	}
	header, err := credentials.cache.headers(ctx, request.URL.String())
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
