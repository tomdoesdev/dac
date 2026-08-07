package dac

import (
	"errors"
	"testing"

	"github.com/tomdoesdev/dac/internal/fault"
)

// TestContentErrorKeepsATransferFailureOnTheNetwork covers the split that
// contentError exists to make. A stall guard is only installed once the response
// headers have arrived, so a stalled transfer can reach this classifier by no
// other route than the body read -- and reporting it as bytes that failed their
// check would send somebody looking at the publisher for a network fault.
func TestContentErrorKeepsATransferFailureOnTheNetwork(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
		code string
	}{
		{name: "stall", err: ErrStalled, code: "timeout"},
		{name: "reset", err: errors.New("connection reset by peer"), code: "network_error"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			value := fault.As(contentError(&transferError{Err: testCase.err}))
			if value.Code != testCase.code {
				t.Fatalf("code = %q, want %q", value.Code, testCase.code)
			}
		})
	}
}

// TestContentErrorSeparatesTheBytesFromTheStore pins the other two outcomes of
// one Put call: bytes that arrived and failed their check, and a store that could
// not keep them.
func TestContentErrorSeparatesTheBytesFromTheStore(t *testing.T) {
	mismatch := &ContentError{ExpectedDigest: "sha256:a", ActualDigest: "sha256:b", ExpectedSize: 2, ActualSize: 2}
	if value := fault.As(contentError(mismatch)); value.Code != "content_mismatch" {
		t.Fatalf("code = %q, want content_mismatch", value.Code)
	}
	if value := fault.As(contentError(errors.New("no space left on device"))); value.Code != "cache_write_failed" {
		t.Fatalf("code = %q, want cache_write_failed", value.Code)
	}
}
