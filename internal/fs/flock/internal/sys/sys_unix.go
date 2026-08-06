// The list names each platform whose syscall package declares Flock.
// Both aix and solaris are Unix and belong to neither list: the aix port
// declares FcntlFlock, which locks byte ranges and has other semantics.
//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package sys

import (
	"errors"
	"syscall"
)

// Lock takes a lock on the descriptor.
// A shared lock admits other shared holders and excludes an exclusive holder.
// A wait of false returns ErrWouldBlock instead of a wait.
func Lock(descriptor uintptr, shared, wait bool) error {
	how := syscall.LOCK_EX
	if shared {
		how = syscall.LOCK_SH
	}
	if !wait {
		how |= syscall.LOCK_NB
	}
	err := syscall.Flock(int(descriptor), how)
	// The two names are one value on Linux and two values elsewhere.
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return ErrWouldBlock
	}
	return err
}

// Unlock releases the lock on the descriptor.
func Unlock(descriptor uintptr) error {
	return syscall.Flock(int(descriptor), syscall.LOCK_UN)
}
