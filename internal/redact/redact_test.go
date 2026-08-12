package redact

import (
	"errors"
	"fmt"
	"testing"
)

// TestURLsRemovesCredentials keeps diagnostics from becoming a secret
// exfiltration path. User info, query parameters, and fragments commonly carry
// credentials, while the scheme, host, and path are still useful for debugging.
func TestURLsRemovesCredentials(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    string
	}{
		{
			name:    "userinfo query and fragment",
			message: "request https://user:password@example.com/artifact?token=secret#fragment failed",
			want:    "request https://example.com/artifact failed",
		},
		{
			// A URL that will not parse is replaced wholesale rather than
			// partially cleaned, so an unrecognized form cannot leak by default.
			name:    "unparseable url becomes a placeholder",
			message: `Get http://%zz/artifact failed`,
			want:    `Get remote URL failed`,
		},
		{
			name:    "a quoted url keeps its closing quote",
			message: `Get "https://example.com/a?token=secret": refused`,
			want:    `Get "https://example.com/a": refused`,
		},
		{
			name:    "text without a url is untouched",
			message: "downloaded bytes do not match expected digest",
			want:    "downloaded bytes do not match expected digest",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := URLs(test.message); got != test.want {
				t.Fatalf("URLs = %q, want %q", got, test.want)
			}
		})
	}
}

// TestErrorRedactsMessageButKeepsChain is the property that lets redaction move
// to the point a third-party error is wrapped: the rendered text is safe, while
// errors.Is still reaches the original cause for classification.
func TestErrorRedactsMessageButKeepsChain(t *testing.T) {
	cause := errors.New(`Get "https://example.com/a?token=secret": connection refused`)
	wrapped := Error(cause)

	if got, want := wrapped.Error(), `Get "https://example.com/a": connection refused`; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if !errors.Is(wrapped, cause) {
		t.Fatal("redacted error no longer unwraps to its cause")
	}

	sentinel := errors.New("transport")
	joined := fmt.Errorf("%w: %w", sentinel, wrapped)
	if !errors.Is(joined, sentinel) || !errors.Is(joined, cause) {
		t.Fatal("wrapping a redacted error broke classification")
	}
	if got := joined.Error(); got != "transport: "+wrapped.Error() {
		t.Fatalf("joined message = %q, want the redacted form", got)
	}
}

// TestErrorPreservesNil keeps the helper safe on cleanup paths where the
// operation being wrapped may have succeeded.
func TestErrorPreservesNil(t *testing.T) {
	if err := Error(nil); err != nil {
		t.Fatalf("Error(nil) = %v, want nil", err)
	}
}
