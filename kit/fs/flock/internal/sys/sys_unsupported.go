// This list is the negation of the list in sys_unix.go.
//go:build !(darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd)

package sys

import "errors"

// Lock reports that this platform has no flock(2).
func Lock(descriptor uintptr, shared, wait bool) error {
	return errors.ErrUnsupported
}

// Unlock reports that this platform has no flock(2).
func Unlock(descriptor uintptr) error {
	return errors.ErrUnsupported
}
