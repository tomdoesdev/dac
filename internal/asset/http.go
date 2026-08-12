package asset

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var environmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateHeaders rejects request forms that would interfere with the HTTP
// transport or make secrets harder to keep out of persistent state.
func ValidateHeaders(headers map[string]string) error {
	seen := make(map[string]struct{}, len(headers))
	for name, value := range headers {
		if !validHeaderName(name) {
			return fmt.Errorf("invalid header name %q", name)
		}
		canonical := strings.ToLower(name)
		if _, exists := seen[canonical]; exists {
			return fmt.Errorf("duplicate header name %q", name)
		}
		seen[canonical] = struct{}{}
		if !utf8.ValidString(value) {
			return fmt.Errorf("header %q must be valid UTF-8", name)
		}
		switch canonical {
		case "host", "content-length", "transfer-encoding", "connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "upgrade", "user-agent":
			return fmt.Errorf("header %q is not allowed", name)
		}
		if strings.HasPrefix(value, "env:") && !environmentPattern.MatchString(strings.TrimPrefix(value, "env:")) {
			return fmt.Errorf("header %q has an invalid environment variable reference", name)
		}
		if !strings.HasPrefix(value, "env:") && strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("header %q contains an invalid line break", name)
		}
	}
	return nil
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", character) {
			continue
		}
		return false
	}
	return true
}

// requestHeaders resolves environment indirections immediately before a
// request. The returned map is ephemeral and must never enter errors or output.
func requestHeaders(headers map[string]string) (http.Header, error) {
	if err := ValidateHeaders(headers); err != nil {
		return nil, err
	}
	result := make(http.Header, len(headers))
	for name, value := range headers {
		if environment, found := strings.CutPrefix(value, "env:"); found {
			resolved, exists := os.LookupEnv(environment)
			if !exists {
				return nil, fmt.Errorf("requires environment variable %s", environment)
			}
			value = resolved
		}
		result.Set(name, value)
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
		return errors.New("too many redirects")
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
