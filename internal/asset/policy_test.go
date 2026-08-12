package asset

import (
	"errors"
	"testing"
	"time"
)

func TestParseMaxSize(t *testing.T) {
	tests := map[string]int64{
		"0": 0, "42": 42, "2KB": 2000, "3MiB": 3 << 20, "4gib": 4 << 30,
	}
	for input, want := range tests {
		if got, err := ParseMaxSize(input); err != nil || got != want {
			t.Errorf("ParseMaxSize(%q) = %d, %v; want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"", "-1", "1.5GiB", "12watts", "999999999999999999999TiB"} {
		if _, err := ParseMaxSize(input); !errors.Is(err, ErrInvalidMaxSize) {
			t.Errorf("ParseMaxSize(%q) error = %v, want ErrInvalidMaxSize", input, err)
		}
	}
}

func TestParseIdleTimeout(t *testing.T) {
	for input, want := range map[string]time.Duration{"0": 0, "250ms": 250 * time.Millisecond, "2m": 2 * time.Minute} {
		if got, err := ParseIdleTimeout(input); err != nil || got != want {
			t.Errorf("ParseIdleTimeout(%q) = %s, %v; want %s", input, got, err, want)
		}
	}
	for _, input := range []string{"", "-1s", "tomorrow"} {
		if _, err := ParseIdleTimeout(input); !errors.Is(err, ErrInvalidIdleTimeout) {
			t.Errorf("ParseIdleTimeout(%q) error = %v, want ErrInvalidIdleTimeout", input, err)
		}
	}
}
