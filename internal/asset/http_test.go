package asset

import (
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/tomdoesdev/dac/internal/secret"
)

// TestValidateHeadersClassifiesFailures ensures callers can distinguish
// malformed configuration from headers controlled by the transport.
func TestValidateHeadersClassifiesFailures(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		want    error
	}{
		{
			name:    "invalid name",
			headers: map[string]string{"bad header": "value"},
			want:    ErrInvalidHeader,
		},
		{
			name:    "duplicate name",
			headers: map[string]string{"X-Token": "one", "x-token": "two"},
			want:    ErrInvalidHeader,
		},
		{
			name:    "disallowed name",
			headers: map[string]string{"Host": "example.com"},
			want:    ErrDisallowedHeader,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateHeaders(test.headers)
			if !errors.Is(err, test.want) {
				t.Fatalf("ValidateHeaders() error = %v, want error matching %v", err, test.want)
			}
		})
	}
}

// TestRequestHeadersClassifiesMissingEnvironment keeps absent secrets
// distinguishable from malformed header configuration.
func TestRequestHeadersClassifiesMissingEnvironment(t *testing.T) {
	const environment = "DAC_TEST_MISSING_HEADER_VALUE"
	t.Setenv(environment, "present")
	if err := os.Unsetenv(environment); err != nil {
		t.Fatal(err)
	}
	_, err := requestHeaders(map[string]string{"Authorization": "env:" + environment})
	if !errors.Is(err, secret.ErrMissingValue) {
		t.Fatalf("requestHeaders() error = %v, want error matching %v", err, secret.ErrMissingValue)
	}
}

// TestRedirectPolicyStripsConfiguredHeadersAcrossOrigins protects the HTTP
// trust boundary. Headers copied from DAC's configured request may contain
// credentials, so redirects to another origin must drop them while retaining
// ordinary client metadata such as User-Agent.
func TestRedirectPolicyStripsConfiguredHeadersAcrossOrigins(t *testing.T) {
	origin, err := http.NewRequest(http.MethodGet, "https://origin.example/artifact", nil)
	if err != nil {
		t.Fatal(err)
	}
	origin.Header.Set("Authorization", "secret")
	origin.Header.Set("User-Agent", "dac/test")
	target, err := http.NewRequest(http.MethodGet, "https://target.example/artifact", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := redirectPolicy(target, []*http.Request{origin}); err != nil {
		t.Fatal(err)
	}
	if got := target.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization survived cross-origin redirect: %q", got)
	}
	if got := target.Header.Get("User-Agent"); got != "dac/test" {
		t.Fatalf("User-Agent = %q, want dac/test", got)
	}
}

// TestRedirectPolicyClassifiesRedirectLimit makes the retry boundary visible
// to callers without requiring them to match an error string.
func TestRedirectPolicyClassifiesRedirectLimit(t *testing.T) {
	if err := redirectPolicy(nil, make([]*http.Request, 10)); !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("redirectPolicy() error = %v, want error matching %v", err, ErrTooManyRedirects)
	}
}
