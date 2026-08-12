package fault

import (
	"errors"
	"testing"
)

// TestErrorConstruction protects classification, structured context, and
// standard-library unwrapping through the public creation API.
func TestErrorConstruction(t *testing.T) {
	cause := errors.New("digest mismatch")
	err := NewIntegrityError(
		cause,
		WithAsset("tool"),
		WithHint("run `dac lock tool`"),
		WithIntegrity("expected", "received"),
	)

	var operation *Error
	if !errors.As(err, &operation) {
		t.Fatalf("error type = %T, want *Error", err)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("error does not wrap cause %v", cause)
	}
	if operation.Kind() != ErrorKindIntegrity {
		t.Errorf("kind = %q, want %q", operation.Kind(), ErrorKindIntegrity)
	}
	if operation.Message() != cause.Error() {
		t.Errorf("message = %q, want %q", operation.Message(), cause)
	}
	if operation.Asset() != "tool" {
		t.Errorf("asset = %q, want %q", operation.Asset(), "tool")
	}
	if operation.Hint() != "run `dac lock tool`" {
		t.Errorf("hint = %q, want recovery guidance", operation.Hint())
	}
	if operation.Expected() != "expected" || operation.Received() != "received" {
		t.Errorf("integrity values = (%q, %q), want (%q, %q)", operation.Expected(), operation.Received(), "expected", "received")
	}
	if got := operation.Error(); got != "integrity for asset \"tool\": digest mismatch" {
		t.Errorf("Error() = %q", got)
	}
}

// TestCategoryConstructors ensures every semantic constructor selects its
// documented stable output category.
func TestCategoryConstructors(t *testing.T) {
	cause := errors.New("failure")
	tests := []struct {
		name string
		new  func(error, ...ErrorOption) error
		want ErrorKind
	}{
		{name: "configuration", new: NewConfigurationError, want: ErrorKindConfiguration},
		{name: "filesystem", new: NewFilesystemError, want: ErrorKindFilesystem},
		{name: "integrity", new: NewIntegrityError, want: ErrorKindIntegrity},
		{name: "network", new: NewNetworkError, want: ErrorKindNetwork},
		{name: "cancelled", new: NewCancelledError, want: ErrorKindCancelled},
		{name: "unsupported", new: NewUnsupportedError, want: ErrorKindUnsupported},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var operation *Error
			if err := test.new(cause); !errors.As(err, &operation) {
				t.Fatalf("error type = %T, want *Error", err)
			}
			if operation.Kind() != test.want {
				t.Errorf("kind = %q, want %q", operation.Kind(), test.want)
			}
		})
	}
}

// TestErrorConstructionPreservesNil lets callers wrap optional cleanup errors
// without manufacturing a non-nil project error on successful cleanup.
func TestErrorConstructionPreservesNil(t *testing.T) {
	if err := NewFilesystemError(nil); err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
}
