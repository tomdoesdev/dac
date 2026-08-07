// Package httpclient fetches remote asset bytes.
//
// The transfer itself -- retries, stall detection, split range downloads,
// redirect limits -- belongs to kit/http/getit. This package is the
// adapter between that engine and the application.Fetcher boundary, and it
// holds only what is DAC's own decision: which URLs may be requested, what an
// asset is called, and which failures are worth asking about again.
package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/debug"
	"github.com/tomdoesdev/dac/internal/urlpolicy"
	"github.com/tomdoesdev/kit/fs/util/filename"
	"github.com/tomdoesdev/kit/http/getit"
)

const maxRedirects = 10

var _ application.RequestDetail = (*RequestError)(nil)
var _ application.UpstreamProber = (*Client)(nil)

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
	getter *getit.Getter
}

// New creates an HTTP asset client.
func New(options Options) *Client {
	options.Parallelism = max(options.Parallelism, 1)
	// The URL rule is the outermost stage, so a redirect to a URL DAC will not request is
	// refused before anything under it -- the trust gate included -- is asked about its host.
	decorators := make([]getit.TransportDecorator, 0, len(options.TransportDecorators)+1)
	decorators = append(decorators, checkURL)
	for _, decorator := range options.TransportDecorators {
		decorators = append(decorators, getit.TransportDecorator(decorator))
	}
	return &Client{getter: getit.New(
		getit.WithConnectTimeout(options.Timeout),
		// One asset may split across Parallelism requests, and the whole client may not
		// exceed that many range requests at once however many assets are running.
		getit.WithBudget(options.Parallelism),
		getit.WithMaxRedirects(maxRedirects),
		getit.WithRetryPolicy(retryable),
		getit.WithLogger(debug.Or(options.Logger)),
		getit.WithTransportDecorator(decorators...),
		getit.WithDefaults(
			getit.WithParts(options.Parallelism),
			getit.WithRetries(options.Retries),
			// A large asset over a slow link is slow rather than broken, so the timeout
			// bounds silence rather than the transfer.
			getit.WithStallTimeout(options.Timeout),
		),
	)}
}

// Close releases idle connections.
func (client *Client) Close() { client.getter.Close() }

// Fetch sends an unconditional or conditional asset request.
func (client *Client) Fetch(ctx context.Context, request application.FetchRequest) (*application.FetchResponse, error) {
	// The URL a caller named is checked before a request goes out at all, so a URL DAC
	// will not request costs nothing. Every later hop is checked by the transport stage.
	parsed, err := urlpolicy.ParseAndCheck(request.URL, request.AllowInsecureHTTP)
	if err != nil {
		return nil, &RequestError{URL: request.URL, Err: err}
	}
	var options []getit.RequestOption
	validator := getit.Validator{ETag: request.ETag}
	if request.LastModified != "" {
		modified, parseErr := http.ParseTime(request.LastModified)
		if parseErr != nil {
			return nil, &RequestError{URL: request.URL, Err: getit.Permanent(parseErr)}
		}
		validator.LastModified = modified
	}
	if !validator.Empty() {
		options = append(options, getit.WithValidator(validator))
	}
	response, err := client.getter.Get(withInsecure(ctx, request.AllowInsecureHTTP), parsed.String(), options...)
	if err != nil {
		return nil, requestError(parsed.String(), err)
	}
	return &application.FetchResponse{
		NotModified:  response.NotModified,
		ETag:         response.Validator.ETag,
		LastModified: lastModifiedText(response.Validator),
		Filename:     responseFilename(response),
		Length:       response.Length,
		Body:         &assetBody{inner: response.Body},
	}, nil
}

// Probe sends an unconditional HEAD through the same policies as an asset GET.
func (client *Client) Probe(ctx context.Context, request application.ProbeRequest) (*application.ProbeResponse, error) {
	parsed, err := urlpolicy.ParseAndCheck(request.URL, request.AllowInsecureHTTP)
	if err != nil {
		return nil, &RequestError{URL: request.URL, Err: err}
	}
	response, err := client.getter.Head(withInsecure(ctx, request.AllowInsecureHTTP), parsed.String())
	if err != nil {
		return nil, requestError(parsed.String(), err)
	}
	defer func() { _ = response.Body.Close() }()
	return &application.ProbeResponse{
		ETag:         response.Validator.ETag,
		LastModified: lastModifiedText(response.Validator),
		Length:       response.Length,
	}, nil
}

// lastModifiedText converts an HTTP validator to the canonical lock-file form.
func lastModifiedText(validator getit.Validator) string {
	if validator.LastModified.IsZero() {
		return ""
	}
	return validator.LastModified.UTC().Format(http.TimeFormat)
}

// insecureKey carries one asset's permission to be fetched over plain HTTP.
//
// The permission is a property of the asset rather than of the client, and the URL rule has
// to hold for every hop, so it travels on the request context: that is the one thing the
// first request, each redirect it follows, and each range of a split all still have in common
// by the time they reach the transport.
type insecureKey struct{}

func withInsecure(ctx context.Context, allow bool) context.Context {
	return context.WithValue(ctx, insecureKey{}, allow)
}

func allowsInsecure(ctx context.Context) bool {
	allow, _ := ctx.Value(insecureKey{}).(bool)
	return allow
}

// checkURL is the transport stage that applies the URL rule to every request.
// A redirect is where a rule that only looked at the URL a caller gave would be got around.
func checkURL(next http.RoundTripper) http.RoundTripper {
	return &urlGuard{next: next}
}

type urlGuard struct{ next http.RoundTripper }

func (transport *urlGuard) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := urlpolicy.Check(request.URL, allowsInsecure(request.Context())); err != nil {
		// Permanent, or the same refusal is made again on every retry of every range.
		return nil, getit.Permanent(err)
	}
	return transport.next.RoundTrip(request)
}

// retryable reports whether a failure is worth another attempt.
// DAC adds the two refusals of its own that the engine cannot recognize; everything else is
// the engine's rule. A refusal that were retried would be made once per range of every split.
func retryable(err error) bool {
	if errors.Is(err, application.ErrHostNotTrusted) || errors.Is(err, urlpolicy.ErrNotPermitted) {
		return false
	}
	return getit.Retryable(err)
}

// assetBody restates a stalled transfer as the failure the application classifies on.
// The stall arrives through Read, part way through the store hashing the bytes, so it is the
// body rather than the request that has to say so.
type assetBody struct{ inner io.ReadCloser }

func (body *assetBody) Read(buffer []byte) (int, error) {
	count, err := body.inner.Read(buffer)
	// io.EOF is passed back exactly as it arrived, because io.Copy compares it rather than unwrapping it.
	if err != nil && errors.Is(err, getit.ErrStalled) {
		return count, fmt.Errorf("%w: %w", application.ErrStalled, err)
	}
	return count, err
}

func (body *assetBody) Close() error { return body.inner.Close() }

// responseFilename reports the name the origin gives an asset.
// A Content-Disposition header is the origin saying the name outright, so it wins.
func responseFilename(response *getit.Response) string {
	if name := filename.FromDisposition(response.Header.Get("Content-Disposition")); name != "" {
		return name
	}
	// URL is the end of the redirect chain, which is the request that actually answered.
	if response.URL == nil {
		return ""
	}
	return filename.FromURL(response.URL.String())
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

// requestError restates the engine's failure as the one the application reports on.
// The cause is carried through unchanged, so the refusals DAC classifies by -- a trust
// refusal, a URL the policy would not permit -- are still found under it.
func requestError(url string, err error) error {
	var request *getit.RequestError
	if errors.As(err, &request) {
		return &RequestError{URL: request.URL, Status: request.Status, Err: request.Err}
	}
	return &RequestError{URL: url, Err: err}
}
