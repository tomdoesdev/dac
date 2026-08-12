package output

import "testing"

// TestSanitizeErrorRemovesURLSecrets keeps diagnostics from becoming a secret
// exfiltration path. User info, query parameters, and fragments commonly carry
// credentials, while the scheme, host, and path are still useful for debugging.
func TestSanitizeErrorRemovesURLSecrets(t *testing.T) {
	message := "request https://user:password@example.com/artifact?token=secret#fragment failed"
	got := sanitizeError(message)
	if want := "request https://example.com/artifact failed"; got != want {
		t.Fatalf("sanitizeError = %q, want %q", got, want)
	}
}

// TestOptionsRejectJSONAndQuiet pins an intentionally invalid output contract:
// JSON promises machine-readable output while quiet promises no output. DAC
// rejects the ambiguous combination instead of choosing one mode implicitly.
func TestOptionsRejectJSONAndQuiet(t *testing.T) {
	if err := (Options{JSON: true, Quiet: true}).Validate(); err == nil {
		t.Fatal("Validate accepted --json with --quiet")
	}
}
