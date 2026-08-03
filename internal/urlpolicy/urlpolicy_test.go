package urlpolicy

import (
	"errors"
	"net/url"
	"testing"
)

func TestRejectionsAreIdentifiable(t *testing.T) {
	for _, raw := range []string{"http://example.com/a", "ftp://example.com/a", "https://user:pw@example.com/a"} {
		parsed, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		// The httpclient tells a permanent policy rejection from a transient
		// network failure through this sentinel, so retries never chase one.
		if err := Check(parsed, false); !errors.Is(err, ErrNotPermitted) {
			t.Fatalf("%q returned %v", raw, err)
		}
	}
}

func TestPermission(t *testing.T) {
	tests := []struct {
		url      string
		allow    bool
		wantFail bool
	}{
		{url: "https://example.com/a"},
		{url: "http://localhost/a"},
		{url: "http://127.0.0.1/a"},
		{url: "http://[::1]/a"},
		// The whole 127 block is loopback, not just the first address in it.
		{url: "http://127.0.0.2/a"},
		// A private address is somebody else's machine. Reaching it in the clear
		// is the case the opt-in exists for, so it needs one.
		{url: "http://192.168.1.1/a", wantFail: true},
		{url: "http://192.168.1.1/a", allow: true},
		{url: "http://example.com/a", wantFail: true},
		{url: "http://example.com/a", allow: true},
		{url: "https://user:password@example.com/a", wantFail: true},
		// Credentials in a URL are refused whatever else permits it, because DAC
		// gets credentials from a helper and nowhere else.
		{url: "https://user:password@example.com/a", allow: true, wantFail: true},
		{url: "http://user@127.0.0.1/a", wantFail: true},
		// An opt-in covers the scheme, never a scheme DAC does not speak.
		{url: "ftp://example.com/a", allow: true, wantFail: true},
		{url: "file:///etc/passwd", allow: true, wantFail: true},
	}
	for _, test := range tests {
		parsed, err := url.Parse(test.url)
		if err != nil {
			t.Fatal(err)
		}
		err = Check(parsed, test.allow)
		if (err != nil) != test.wantFail {
			t.Fatalf("Check(%q, %t) returned %v", test.url, test.allow, err)
		}
	}
}

// TestParseAndCheckAppliesBothHalves covers the entry point every caller that
// starts from a string uses. A URL that will not parse and a URL that parses
// into something DAC refuses are different failures, and neither may come back
// as a usable URL.
func TestParseAndCheckAppliesBothHalves(t *testing.T) {
	if _, err := ParseAndCheck("https://exa mple.com/a", false); err == nil {
		t.Fatal("a URL that does not parse was accepted")
	}
	value, err := ParseAndCheck("http://example.com/a", false)
	if err == nil {
		t.Fatalf("an insecure URL was accepted as %v", value)
	}
	if !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("a policy rejection returned %v", err)
	}
	value, err = ParseAndCheck("https://example.com/geo.bin", false)
	if err != nil {
		t.Fatal(err)
	}
	if value.Host != "example.com" || value.Path != "/geo.bin" {
		t.Fatalf("ParseAndCheck returned %#v", value)
	}
}
