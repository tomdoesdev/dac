package flock

import (
	"io/fs"
	"time"
)

// Option changes how this package takes a lock.
type Option func(*settings)

// settings holds the values that the options change.
type settings struct {
	mode       Mode
	fileMode   fs.FileMode
	dirMode    fs.FileMode
	create     bool
	firstDelay time.Duration
	limitDelay time.Duration
}

// The defaults suit a lock file that belongs to one user.
const (
	defaultFileMode   = 0o600
	defaultDirMode    = 0o755
	defaultFirstDelay = time.Millisecond
	defaultLimitDelay = 32 * time.Millisecond
)

func newSettings(options []Option) settings {
	current := settings{
		mode:       Exclusive,
		fileMode:   defaultFileMode,
		dirMode:    defaultDirMode,
		create:     true,
		firstDelay: defaultFirstDelay,
		limitDelay: defaultLimitDelay,
	}
	for _, option := range options {
		option(&current)
	}
	return current
}

// WithMode selects Exclusive or Shared. The default is Exclusive.
func WithMode(mode Mode) Option {
	return func(current *settings) { current.mode = mode }
}

// WithFileMode sets the mode of a lock file that this package creates.
// The default is 0o600. The mode of a file that exists already does not change.
func WithFileMode(mode fs.FileMode) Option {
	return func(current *settings) { current.fileMode = mode }
}

// WithDirMode sets the mode of a directory that this package creates.
// The default is 0o755.
func WithDirMode(mode fs.FileMode) Option {
	return func(current *settings) { current.dirMode = mode }
}

// WithoutCreate stops this package from making the file and its directory.
// A path that does not exist then fails with fs.ErrNotExist.
func WithoutCreate() Option {
	return func(current *settings) { current.create = false }
}

// WithBackoff sets the first wait and the longest wait between two attempts.
// The default is 1 ms to 32 ms. A value below 1 ns keeps the default.
func WithBackoff(first, limit time.Duration) Option {
	return func(current *settings) {
		if first > 0 {
			current.firstDelay = first
		}
		if limit > 0 {
			current.limitDelay = limit
		}
		// A limit below the first wait would make the backoff count down.
		current.limitDelay = max(current.limitDelay, current.firstDelay)
	}
}
