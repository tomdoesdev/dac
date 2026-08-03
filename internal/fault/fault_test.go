package fault

import (
	"errors"
	"fmt"
	"testing"
)

func TestNewAndWrapCarryStableCodes(t *testing.T) {
	plain := New("lock_stale", "The lock file is stale.")
	if plain.Code != "lock_stale" || plain.Error() != "The lock file is stale." {
		t.Fatalf("unexpected error: %+v", plain)
	}

	cause := errors.New("no such file")
	wrapped := Wrap("cache_read_failed", "DAC could not read the cache.", cause)
	if !errors.Is(wrapped, cause) {
		t.Fatal("Wrap did not preserve the cause")
	}
	if wrapped.Error() != "DAC could not read the cache: no such file" {
		t.Fatalf("unexpected message: %q", wrapped.Error())
	}
}

func TestAsRecognizesAndWrapsErrors(t *testing.T) {
	original := New("lock_stale", "The lock file is stale.")
	if As(original) != original {
		t.Fatal("As did not return the original error")
	}
	// A code must survive wrapping, or the CLI would report internal_error for
	// an error it knows perfectly well.
	nested := fmt.Errorf("while locking: %w", original)
	if got := As(nested); got.Code != "lock_stale" {
		t.Fatalf("wrapped error reported code %q", got.Code)
	}

	unknown := As(errors.New("something else"))
	if unknown.Code != "internal_error" {
		t.Fatalf("unknown error reported code %q", unknown.Code)
	}
	if As(nil).Code != "internal_error" {
		t.Fatal("a nil error must not produce a success code")
	}
}

func TestErrorJoinsAMessageAndCauseAsOneSentence(t *testing.T) {
	err := Wrap("network_error", "The asset request failed.", errors.New("connection refused"))
	if got, want := err.Error(), "The asset request failed: connection refused"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if got := New("asset_exists", "The manifest already has this asset.").Error(); got != "The manifest already has this asset." {
		t.Fatalf("a causeless error lost its full stop: %q", got)
	}
}
