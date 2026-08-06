// Package sys calls flock(2).
//
// This package holds every build constraint in the flock module. The lock
// logic and the public API build on all platforms, so a platform that has no
// flock(2) cannot make the API and the stub disagree.
package sys

import "errors"

// ErrWouldBlock reports that another holder has the lock.
// The lockfile package turns this into the error that callers see.
var ErrWouldBlock = errors.New("the file is locked")
