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

// Options configures one HTTP client.
type Options struct {
	Timeout time.Duration
	Retries int
	// Rewriter decides the URL DAC requests for a canonical asset URL.
	Rewriter *rewrite.Config
	// Credentials supplies request headers for hosts that need them.
	Credentials *credential.Resolver
}

// Client owns the connections used for asset requests.
type Client struct {
	options   Options
	transport *http.Transport
}

// New creates an HTTP asset client.
func New(options Options) *Client {
	return &Client{
		options: options,
		transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: options.Timeout, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   8,
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
		return nil, err
	}
	if target.Rewritten && target.AllowInsecureHTTP {
		request.AllowInsecureHTTP = true
	}
	parsed, err := urlpolicy.ParseAndCheck(target.URL, request.AllowInsecureHTTP)
	if err != nil {
		return nil, err
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
			return nil, lastErr
		}
		if err := sleep(ctx, backoff(attempt, err)); err != nil {
			return nil, err
		}
	}
}

func (client *Client) attempt(ctx context.Context, input application.FetchRequest) (*application.FetchResponse, error) {
	requestCtx, cancel := context.WithCancel(ctx)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, input.URL, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	request.Header.Set("Accept-Encoding", "identity")
	if input.ETag != "" {
		request.Header.Set("If-None-Match", input.ETag)
	}
	if err := client.authorize(requestCtx, request); err != nil {
		cancel()
		return nil, err
	}

	httpClient := &http.Client{Transport: client.transport, CheckRedirect: client.checkRedirect(requestCtx, input.AllowInsecureHTTP)}
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
		Body:        guard(response.Body, cancel, client.options.Timeout),
	}, nil
}

// checkRedirect applies URL policy, host policy, and credential rules to each
// redirect target.
func (client *Client) checkRedirect(ctx context.Context, allowInsecure bool) func(*http.Request, []*http.Request) error {
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
		return client.authorize(ctx, request)
	}
}

// authorize sets the credentials for one request host. It clears any header
// carried over from a previous hop first, so a redirect never forwards one
// host's credentials to another.
func (client *Client) authorize(ctx context.Context, request *http.Request) error {
	request.Header.Del("Authorization")
	header, err := client.options.Credentials.Headers(ctx, request.URL.String())
	if err != nil {
		return err
	}
	if len(header) > 0 {
		for name, values := range header {
			request.Header.Del(name)
			for _, value := range values {
				request.Header.Add(name, value)
			}
		}
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
