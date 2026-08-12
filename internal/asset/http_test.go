package asset

import (
	"net/http"
	"testing"
)

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
