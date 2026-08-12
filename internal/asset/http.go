package asset

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tomdoesdev/dac/internal/secret"
)

var (
	// ErrInvalidHeader marks a header name or value that cannot be sent safely.
	ErrInvalidHeader = errors.New("invalid HTTP header")
	// ErrDisallowedHeader marks a transport-controlled header supplied by configuration.
	ErrDisallowedHeader = errors.New("disallowed HTTP header")
	// ErrTooManyRedirects marks a request that exceeded dac's redirect limit.
	ErrTooManyRedirects = errors.New("too many redirects")
)

// headerError preserves the affected header and a human-readable explanation
// while allowing callers to classify it with errors.Is.
type headerError struct {
	kind   error
	name   string
	detail string
}

func (err *headerError) Error() string {
	return fmt.Sprintf("%v %q: %s", err.kind, err.name, err.detail)
}

func (err *headerError) Unwrap() error {
	return err.kind
}

// newHeaderError consistently attaches context to a classifiable header error.
func newHeaderError(kind error, name, detail string) error {
	return &headerError{kind: kind, name: name, detail: detail}
}

var headerNamePattern = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]+$")

var transportManagedHeaderNames = map[string]struct{}{
	"connection":          {},
	"content-length":      {},
	"host":                {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
	"user-agent":          {},
}

// ValidateHeaders rejects request forms that would interfere with the HTTP
// transport or make secrets harder to keep out of persistent state.
func ValidateHeaders(headers map[string]string) error {
	seen := make(map[string]struct{}, len(headers))
	for name, value := range headers {
		if err := validateHeader(name, value, seen); err != nil {
			return err
		}
	}
	return nil
}

// ValidateHeaderName applies the grammar and transport-ownership rules to one
// header name. Callers that edit headers by name only, such as a removal flag,
// use it instead of validating a synthetic single-entry map.
func ValidateHeaderName(name string) error {
	if !validHeaderName(name) {
		return newHeaderError(ErrInvalidHeader, name, "name is invalid")
	}
	if _, managed := transportManagedHeaderNames[strings.ToLower(name)]; managed {
		return newHeaderError(ErrDisallowedHeader, name, "managed by the HTTP transport")
	}
	return nil
}

// validateHeader checks one configured header while tracking names that are
// equivalent under HTTP's case-insensitive matching rules.
func validateHeader(name, value string, seen map[string]struct{}) error {
	if err := ValidateHeaderName(name); err != nil {
		return err
	}

	canonicalName := strings.ToLower(name)
	if _, exists := seen[canonicalName]; exists {
		return newHeaderError(ErrInvalidHeader, name, "duplicates another header name")
	}
	seen[canonicalName] = struct{}{}

	if !utf8.ValidString(value) {
		return newHeaderError(ErrInvalidHeader, name, "value must be valid UTF-8")
	}

	return validateHeaderValue(name, value)
}

// validateHeaderValue distinguishes a secret reference from a literal value
// because the two forms have different valid character sets. The reference
// scheme itself belongs to the configuration layer, not to HTTP.
func validateHeaderValue(name, value string) error {
	if secret.IsReference(value) {
		if err := secret.Validate(value); err != nil {
			return newHeaderError(ErrInvalidHeader, name, err.Error())
		}
		return nil
	}
	if strings.ContainsAny(value, "\r\n") {
		return newHeaderError(ErrInvalidHeader, name, "value contains an invalid line break")
	}
	return nil
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	return headerNamePattern.MatchString(value)
}

// requestHeaders resolves secret references immediately before a request. The
// returned map is ephemeral and must never enter errors, output, or state.
func requestHeaders(headers map[string]string) (http.Header, error) {
	if err := ValidateHeaders(headers); err != nil {
		return nil, err
	}
	result := make(http.Header, len(headers))
	for name, value := range headers {
		resolved, err := secret.Resolve(value)
		if err != nil {
			return nil, err
		}
		result.Set(name, resolved)
	}
	return result, nil
}

// NewHTTPClient returns dac's deliberately small HTTP client. Timeouts cover
// setup and response headers but not legitimate long-running body streams.
func NewHTTPClient() *http.Client {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&netDialer{timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return &http.Client{Transport: transport, CheckRedirect: redirectPolicy}
}

// netDialer narrows net.Dialer to the method http.Transport needs while keeping
// timeout construction close to the client policy above.
type netDialer struct{ timeout time.Duration }

func (dialer *netDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return (&net.Dialer{Timeout: dialer.timeout}).DialContext(ctx, network, address)
}

func redirectPolicy(request *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return ErrTooManyRedirects
	}
	if len(via) == 0 {
		return nil
	}
	userAgent := via[0].Header.Get("User-Agent")
	if sameOrigin(via[0].URL, request.URL) {
		request.Header = via[0].Header.Clone()
	} else {
		request.Header = make(http.Header)
		// User-Agent is dac-controlled rather than manifest-controlled, so it
		// remains safe and useful after stripping configured credentials.
		request.Header.Set("User-Agent", userAgent)
	}
	return nil
}

func sameOrigin(left, right *url.URL) bool {
	return left.Scheme == right.Scheme && left.Host == right.Host
}
