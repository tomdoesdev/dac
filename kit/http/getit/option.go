package getit

import (
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"time"
)

// Option changes how a Getter works. Options apply at New and do not change
// after it, which is what makes one Getter safe to share.
type Option func(*settings)

// RequestOption changes how one Get works.
// A Getter takes its own defaults for these with WithDefaults.
type RequestOption func(*requestSettings)

// TransportDecorator adds request behavior around the transport under it.
type TransportDecorator func(http.RoundTripper) http.RoundTripper

// The defaults suit a caller fetching one file at a time over a public network.
const (
	// defaultChunkSize is the span one range request asks for.
	//
	// It is a fixed size rather than a share of the file, because a split
	// download is delivered in order, so a chunk that arrives early waits in
	// memory for its turn. The size is therefore the memory one part costs.
	defaultChunkSize = 8 << 20
	// defaultBudget bounds the range requests one Getter runs at once.
	defaultBudget = 16
	// defaultConnectTimeout bounds the dial, the TLS handshake, and the wait for
	// response headers.
	defaultConnectTimeout = 30 * time.Second
	// defaultStallTimeout is how long a body may deliver nothing before the
	// request is given up on.
	defaultStallTimeout = 60 * time.Second
	defaultMaxRedirects = 10
	defaultRetries      = 2
	// defaultParts is one request per transfer. Splitting is something a caller
	// asks for: a package that quietly made four requests where one was asked
	// for would be a surprise to the origin as much as to the caller.
	defaultParts = 1
)

type settings struct {
	transport      http.RoundTripper
	decorators     []TransportDecorator
	chunkSize      int64
	budget         int
	connectTimeout time.Duration
	maxRedirects   int
	urlPolicy      func(*url.URL) error
	retryPolicy    func(error) bool
	logger         *slog.Logger
	request        requestSettings
}

type requestSettings struct {
	hooks        hookList
	validator    Validator
	parts        int
	retries      int
	stallTimeout time.Duration
	maxBytes     int64
	header       http.Header
	split        bool
}

func newSettings(options []Option) settings {
	current := settings{
		chunkSize:      defaultChunkSize,
		budget:         defaultBudget,
		connectTimeout: defaultConnectTimeout,
		maxRedirects:   defaultMaxRedirects,
		urlPolicy:      CheckURL,
		retryPolicy:    Retryable,
		logger:         slog.New(slog.DiscardHandler),
		request: requestSettings{
			parts:        defaultParts,
			retries:      defaultRetries,
			stallTimeout: defaultStallTimeout,
			split:        true,
		},
	}
	for _, option := range options {
		option(&current)
	}
	return current
}

// newRequestSettings applies one call's options over the Getter's defaults.
func newRequestSettings(defaults requestSettings, options []RequestOption) requestSettings {
	current := defaults.clone()
	for _, option := range options {
		option(&current)
	}
	current.parts = max(current.parts, 1)
	return current
}

// clone copies the defaults so that one call cannot reach into another.
// The hooks and the headers are shared structures, and appending to either in
// place would write into the Getter that every other call is reading.
func (current requestSettings) clone() requestSettings {
	copied := current
	copied.hooks = slices.Clone(current.hooks)
	copied.header = maps.Clone(current.header)
	return copied
}

// WithDefaults sets the request options that a Getter applies to every Get.
// One call may still override any of them.
func WithDefaults(options ...RequestOption) Option {
	return func(current *settings) {
		for _, option := range options {
			option(&current.request)
		}
	}
}

// WithTransport replaces the transport this package builds.
// Use it to share a connection pool, or to answer requests in a test. The
// decorators from WithTransportDecorator still wrap what is given here.
func WithTransport(transport http.RoundTripper) Option {
	return func(current *settings) { current.transport = transport }
}

// WithTransportDecorator adds request behavior around the transport.
//
// A decorator sees every request the package makes: the first one, each
// redirect it follows, and each range of a split download. That makes it the
// place for a rule that has to hold for all of them, such as credentials, a
// rate limit, or a refusal.
//
// A decorator added first is the outermost, so it is the first to see a request
// and the last to see a response. A decorator that refuses a request should
// wrap its error with Permanent, or the refusal is made again on every retry.
func WithTransportDecorator(decorators ...TransportDecorator) Option {
	return func(current *settings) {
		current.decorators = append(current.decorators, decorators...)
	}
}

// WithChunkSize sets the span one range request asks for.
// The default is 8 MiB. A size below 1 keeps the default. A split download
// holds up to one chunk per part in memory, so this is what parallelism costs.
func WithChunkSize(size int64) Option {
	return func(current *settings) {
		if size > 0 {
			current.chunkSize = size
		}
	}
}

// WithBudget bounds the range requests one Getter runs at once, across every
// transfer. The default is 16.
//
// It stops the parts of one transfer and the number of concurrent transfers
// from multiplying into more connections than a caller meant to open. A
// transfer that cannot get the parts it asked for takes what is free, and one
// that can get none streams the body over its first response instead of
// waiting. A value below 1 removes the ceiling.
func WithBudget(count int) Option {
	return func(current *settings) { current.budget = count }
}

// WithConnectTimeout bounds the dial, the TLS handshake, and the wait for
// response headers. The default is 30 seconds. Zero means no timeout.
//
// It does not bound the body. That is WithStallTimeout, because a large file
// over a slow link is slow rather than broken.
func WithConnectTimeout(timeout time.Duration) Option {
	return func(current *settings) { current.connectTimeout = timeout }
}

// WithMaxRedirects sets how many redirects one request may follow.
// The default is 10. A value below 1 follows none.
func WithMaxRedirects(count int) Option {
	return func(current *settings) { current.maxRedirects = count }
}

// WithURLPolicy replaces the rule that decides which URLs may be requested.
//
// The rule applies to the URL given to Get and to the target of every redirect
// it follows, which is where a policy that only checked the first URL would be
// got around. The default is CheckURL. A policy that returns an error refuses
// the request, and the refusal is permanent.
func WithURLPolicy(policy func(*url.URL) error) Option {
	return func(current *settings) {
		if policy != nil {
			current.urlPolicy = policy
		}
	}
}

// WithRetryPolicy replaces the rule that decides whether a failure is worth
// another attempt. The default is Retryable, which a policy can still call for
// the failures it has no opinion about:
//
//	getit.WithRetryPolicy(func(err error) bool {
//		if errors.Is(err, errNoQuota) {
//			return false
//		}
//		return getit.Retryable(err)
//	})
func WithRetryPolicy(policy func(error) bool) Option {
	return func(current *settings) {
		if policy != nil {
			current.retryPolicy = policy
		}
	}
}

// WithLogger traces what each transfer did. The default traces nothing.
// Nothing written here is part of any contract.
func WithLogger(logger *slog.Logger) Option {
	return func(current *settings) {
		if logger != nil {
			current.logger = logger
		}
	}
}

// WithHooks reports what one transfer is doing while it does it.
// Hooks given to a Getter with WithDefaults and hooks given to one Get both
// fire, in the order they were added.
func WithHooks(hooks Hooks) RequestOption {
	return func(current *requestSettings) {
		current.hooks = append(current.hooks, hooks)
	}
}

// WithValidator asks the origin whether the bytes the caller holds are still
// current, using the entity tag and the modification time it reported before.
//
// The origin answers either with the body or with a response whose NotModified
// is set and whose body is empty. Close the body in both cases.
func WithValidator(validator Validator) RequestOption {
	return func(current *requestSettings) { current.validator = validator }
}

// WithParts sets how many requests one transfer may split its body across.
// The default is 1, which never splits.
//
// A split needs the origin to serve byte ranges, to declare a length, and to
// give a validator strong enough to tie the ranges to one set of bytes. Where
// any of that is missing the body is streamed over one response instead, so
// this is a ceiling and not a promise. See also WithBudget.
func WithParts(count int) RequestOption {
	return func(current *requestSettings) { current.parts = max(count, 1) }
}

// WithoutSplit streams the body over one response, whatever WithParts allows.
// It is the way for one call to opt out of a Getter whose defaults split.
func WithoutSplit() RequestOption {
	return func(current *requestSettings) { current.split = false }
}

// WithRetries sets how many further attempts one request gets. The default is 2.
// Each range of a split download is retried on its own, because a whole body
// that fails halfway has already been handed to the caller and cannot be taken
// back.
func WithRetries(count int) RequestOption {
	return func(current *requestSettings) { current.retries = max(count, 0) }
}

// WithStallTimeout sets how long a body may deliver nothing before the request
// is given up on. The default is 60 seconds. Zero means no limit.
//
// It is an idle timeout and not a deadline: the clock restarts on every read
// that returns bytes, so a slow transfer runs to the end and a dead one does
// not hold the caller open. A body that stalls fails with ErrStalled.
func WithStallTimeout(timeout time.Duration) RequestOption {
	return func(current *requestSettings) { current.stallTimeout = timeout }
}

// WithMaxBytes bounds the body one transfer accepts. Zero, the default, is no
// bound. A body that passes it fails with ErrTooLarge.
//
// The bound is checked against the declared length before any body is read, and
// against the bytes that arrive, because a declared length is a claim.
func WithMaxBytes(count int64) RequestOption {
	return func(current *requestSettings) { current.maxBytes = count }
}

// WithHeader adds a header to every request one transfer makes.
//
// This package sets Accept-Encoding, Range, and the conditional headers itself,
// and its values replace anything set here. Everything else is passed through.
func WithHeader(name, value string) RequestOption {
	return func(current *requestSettings) {
		if current.header == nil {
			current.header = http.Header{}
		}
		current.header.Add(name, value)
	}
}

// CheckURL is the URL rule this package applies when a caller names no other.
//
// It permits HTTP and HTTPS and refuses a URL that carries credentials, because
// a URL is written to logs and to error messages, and a password in one is a
// password that has leaked. Anything narrower, such as requiring HTTPS or
// naming the hosts that may be reached, belongs to the caller and goes in
// WithURLPolicy.
func CheckURL(value *url.URL) error {
	if value.User != nil {
		return fmt.Errorf("%w: URL credentials are not supported", ErrNotPermitted)
	}
	if value.Scheme != "http" && value.Scheme != "https" {
		return fmt.Errorf("%w: URL scheme must be HTTP or HTTPS", ErrNotPermitted)
	}
	return nil
}
