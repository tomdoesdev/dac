package project

import "errors"

var (
	// ErrProjectNotFound marks discovery that reaches the filesystem root.
	ErrProjectNotFound = errors.New("no dac.toml found in this directory or any parent directory")
	// ErrFlockUnsupported marks platforms without the locking primitive dac needs.
	ErrFlockUnsupported = errors.New("dac requires flock(2) on this platform")
)
