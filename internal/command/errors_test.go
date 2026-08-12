package command

import (
	"errors"
	"testing"
)

// TestSetErrorsAreClassifiable verifies both constant and contextual option
// failures retain stable identities for CLI and API callers.
func TestSetErrorsAreClassifiable(t *testing.T) {
	if _, err := parseSets([]string{"missing-separator"}); !errors.Is(err, ErrInvalidSet) {
		t.Fatalf("invalid set error = %v, want ErrInvalidSet", err)
	}
	if _, err := parseSets([]string{"KEY=first", "KEY=second"}); !errors.Is(err, ErrDuplicateSet) {
		t.Fatalf("duplicate set error = %v, want ErrDuplicateSet", err)
	}
}
