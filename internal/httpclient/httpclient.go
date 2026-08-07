// Package httpclient fetches remote asset bytes.
//
// The transfer itself -- retries, stall detection, split range downloads,
// redirect limits -- belongs to kit/http/getit. This package is the
// adapter between that engine and the dac.Fetcher boundary, and it
// holds only DAC-specific response adaptation: asset naming and error details.
package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/tomdoesdev/dac/internal/dac"
	"github.com/tomdoesdev/dac/internal/debug"
	"github.com/tomdoesdev/kit/fs/util/filename"
	"github.com/tomdoesdev/kit/http/getit"
)

const maxRedirects = 10

var _ dac.RequestDetail = (*RequestError)(nil)
var _ dac.UpstreamProber = (*Client)(nil)

// Options configures one HTTP client.
type Options struct {
	Timeout time.Duration
	Retries int
	// Parallelism is how many requests one asset may be split across when the origin serves byte ranges.
	Parallelism int
	// Logger traces what each transfer did. A nil logger traces nothing.
	Logger *slog.Logger
}

// Client owns the connections used for asset requests.
type Client struct {
	getter *getit.Getter
}

// New creates an HTTP asset client.
func New(options Options) *Client {
	options.Parallelism = max(options.Parallelism, 1)
	return &Client{getter: getit.New(
		getit.WithConnectTimeout(options.Timeout),
		// One asset may split across Parallelism requests, and the whole client may not
		// exceed that many range requests at once however many assets are running.
		getit.WithBudget(options.Parallelism),
		getit.WithMaxRedirects(maxRedirects),
		getit.WithLogger(debug.Or(options.Logger)),
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
func (client *Client) Fetch(ctx context.Context, request dac.FetchRequest) (*dac.FetchResponse, error) {
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
	response, err := client.getter.Get(ctx, request.URL, options...)
	if err != nil {
		return nil, requestError(request.URL, err)
	}
	return &dac.FetchResponse{
		NotModified:  response.NotModified,
		ETag:         response.Validator.ETag,
		LastModified: lastModifiedText(response.Validator),
		Filename:     responseFilename(response),
		Length:       response.Length,
		Body:         &assetBody{inner: response.Body},
	}, nil
}

// Probe sends an unconditional HEAD through the same policies as an asset GET.
func (client *Client) Probe(ctx context.Context, request dac.ProbeRequest) (*dac.ProbeResponse, error) {
	response, err := client.getter.Head(ctx, request.URL)
	if err != nil {
		return nil, requestError(request.URL, err)
	}
	defer func() { _ = response.Body.Close() }()
	return &dac.ProbeResponse{
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

// assetBody restates a stalled transfer as the failure DAC classifies on.
// The stall arrives through Read, part way through the store hashing the bytes, so it is the
// body rather than the request that has to say so.
type assetBody struct{ inner io.ReadCloser }

func (body *assetBody) Read(buffer []byte) (int, error) {
	count, err := body.inner.Read(buffer)
	// io.EOF is passed back exactly as it arrived, because io.Copy compares it rather than unwrapping it.
	if err != nil && errors.Is(err, getit.ErrStalled) {
		return count, fmt.Errorf("%w: %w", dac.ErrStalled, err)
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

// RequestURL and StatusCode satisfy dac.RequestDetail.
func (value *RequestError) RequestURL() string { return value.URL }

func (value *RequestError) StatusCode() int { return value.Status }

// requestError restates the engine's failure as the one DAC reports on.
// The cause is carried through unchanged so callers can inspect the HTTP failure.
func requestError(url string, err error) error {
	var request *getit.RequestError
	if errors.As(err, &request) {
		return &RequestError{URL: request.URL, Status: request.Status, Err: request.Err}
	}
	return &RequestError{URL: url, Err: err}
}
