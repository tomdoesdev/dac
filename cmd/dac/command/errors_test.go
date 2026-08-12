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

func TestHeaderAndSingletonErrorsAreClassifiable(t *testing.T) {
	if _, err := parseHeaders([]string{"missing-separator"}); !errors.Is(err, ErrInvalidHeader) {
		t.Fatalf("invalid header error = %v, want ErrInvalidHeader", err)
	}
	if _, err := parseHeaders([]string{"X-Test=one", "x-test=two"}); !errors.Is(err, ErrDuplicateHeader) {
		t.Fatalf("duplicate header error = %v, want ErrDuplicateHeader", err)
	}
	value := newSingleValue("--url")
	if err := value.Set("one"); err != nil {
		t.Fatal(err)
	}
	if err := value.Set("two"); !errors.Is(err, ErrOptionRepeated) {
		t.Fatalf("repeated singleton error = %v, want ErrOptionRepeated", err)
	}
}
