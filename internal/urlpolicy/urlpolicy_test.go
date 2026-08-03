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
		{url: "http://example.com/a", wantFail: true},
		{url: "http://example.com/a", allow: true},
		{url: "https://user:password@example.com/a", wantFail: true},
		// An opt-in covers the scheme, never a scheme DAC does not speak.
		{url: "ftp://example.com/a", allow: true, wantFail: true},
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
