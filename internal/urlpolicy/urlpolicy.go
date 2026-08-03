// Package urlpolicy decides which asset URLs DAC is willing to request.
//
// The rule belongs to neither the project file nor the HTTP client: the
// manifest applies it when it validates an asset, and the client applies it
// again to every redirect target, so it sits below both of them.
package urlpolicy

import (
	"errors"
	"fmt"
	"net"
	"net/url"
)

// ErrNotPermitted marks a URL that DAC refuses on policy grounds. Callers use
// it to tell a permanent rejection apart from a transient network failure.
var ErrNotPermitted = errors.New("URL is not permitted")

// ParseAndCheck parses a URL and applies the request policy.
func ParseAndCheck(rawURL string, allowInsecure bool) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if err := Check(parsed, allowInsecure); err != nil {
		return nil, err
	}
	return parsed, nil
}

// Check applies the HTTP permission rule to a URL. HTTPS is always permitted,
// and HTTP only for a loopback host or when the asset opts in.
func Check(value *url.URL, allowInsecure bool) error {
	if value.User != nil {
		return fmt.Errorf("%w: URL credentials are not supported", ErrNotPermitted)
	}
	if value.Scheme == "https" {
		return nil
	}
	if value.Scheme != "http" {
		return fmt.Errorf("%w: URL scheme must be HTTPS or HTTP", ErrNotPermitted)
	}
	host := value.Hostname()
	address := net.ParseIP(host)
	if host == "localhost" || address != nil && address.IsLoopback() || allowInsecure {
		return nil
	}
	return fmt.Errorf("%w: HTTP URL requires allowInsecureHttp", ErrNotPermitted)
}
