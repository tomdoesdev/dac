package application

import (
	"errors"
	"testing"

	"github.com/tom/dac/internal/coord"
	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/project"
)

// TestVersionFaultNamesEveryFailedCheck is the guard on a trap rather than on a
// bug. Only one error reaches the version check today, so every caller happens
// to get the collision fault it expects. A converter that returned nil for
// anything else would hand the next caller a zero result and no error, which
// reads as a check that passed.
func TestVersionFaultNamesEveryFailedCheck(t *testing.T) {
	collision := &project.VersionCollisionError{
		Group:    coord.MustParse("app/geo@1").Group(),
		Versions: []string{"1", "2"},
		Digest:   "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}
	if got := fault.As(versionFault(collision)); got.Code != "version_collision" {
		t.Fatalf("a collision reported %q", got.Code)
	}
	other := errors.New("the version check could not run")
	value := versionFault(other)
	if value == nil {
		t.Fatal("a failed version check reported no error")
	}
	if got := fault.As(value); got.Code != "lock_invalid" {
		t.Fatalf("a non-collision reported %q", got.Code)
	}
	if !errors.Is(value, other) {
		t.Fatalf("the fault dropped its cause: %v", value)
	}
}

// TestAsCollisionReportsWhatItRecognized keeps the predicate lockFault relies
// on honest, so that "not a collision" stays distinguishable from "no error".
func TestAsCollisionReportsWhatItRecognized(t *testing.T) {
	if _, found := asCollision(errors.New("lock file is unreadable")); found {
		t.Fatal("an unrelated error was reported as a collision")
	}
	value := lockFault("lock_invalid", "The lock file is invalid.", errors.New("lock file is unreadable"))
	if got := fault.As(value); got.Code != "lock_invalid" {
		t.Fatalf("lockFault reported %q", got.Code)
	}
}
