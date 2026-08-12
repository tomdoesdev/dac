package flock

import (
	"testing"
	"time"
)

// TestDefaultSettings pins the package's conservative lock behavior: exclusive
// acquisition, automatic file creation, and documented file/directory modes.
func TestDefaultSettings(t *testing.T) {
	current := newSettings(nil)
	if current.mode != Exclusive {
		t.Fatalf("the default mode is %v", current.mode)
	}
	if !current.create {
		t.Fatal("the default settings do not create the file")
	}
	if current.fileMode != defaultFileMode || current.dirMode != defaultDirMode {
		t.Fatalf("the default modes are %v and %v", current.fileMode, current.dirMode)
	}
}

// TestRemoveOnReleaseIsOptIn keeps persistent sidecars as the safe default. File
// removal has inode-replacement and shared-holder hazards, so callers must
// request the package's coordinated transient-lock protocol explicitly.
func TestRemoveOnReleaseIsOptIn(t *testing.T) {
	if newSettings(nil).remove {
		t.Fatal("default settings remove persistent lock files")
	}
	if !newSettings([]Option{RemoveOnRelease()}).remove {
		t.Fatal("RemoveOnRelease did not enable removal")
	}
}

// A backoff that counts down would make each wait shorter than the one before
// it, so the limit holds the first wait as its floor.
func TestBackoffLimitCannotFallBelowTheFirstWait(t *testing.T) {
	current := newSettings([]Option{WithBackoff(20*time.Millisecond, time.Millisecond)})
	if current.limitDelay != 20*time.Millisecond {
		t.Fatalf("the limit is %v, want 20ms", current.limitDelay)
	}
}

// TestBackoffKeepsTheDefaultForAnEmptyValue treats zero durations as omitted
// configuration. This avoids a zero-delay busy loop that would burn CPU under
// lock contention.
func TestBackoffKeepsTheDefaultForAnEmptyValue(t *testing.T) {
	current := newSettings([]Option{WithBackoff(0, 0)})
	if current.firstDelay != defaultFirstDelay || current.limitDelay != defaultLimitDelay {
		t.Fatalf("the backoff is %v to %v", current.firstDelay, current.limitDelay)
	}
}
