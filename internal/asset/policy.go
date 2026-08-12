package asset

import "time"

const (
	// DefaultMaxSize bounds an undeclared download while remaining large enough
	// for SDKs, archives, and other package-manager-style artifacts.
	DefaultMaxSize int64 = 4 * 1024 * 1024 * 1024
	// DefaultIdleTimeout stops a body read that makes no progress. It is not a
	// total transfer deadline, so active downloads may run for as long as needed.
	DefaultIdleTimeout = 30 * time.Second
)

// TransferPolicy contains the effective safety limits applied to one request.
// Zero disables the corresponding limit explicitly.
type TransferPolicy struct {
	MaxSize     int64
	IdleTimeout time.Duration
}

// DefaultTransferPolicy returns the limits used when a manifest omits them.
func DefaultTransferPolicy() TransferPolicy {
	return TransferPolicy{MaxSize: DefaultMaxSize, IdleTimeout: DefaultIdleTimeout}
}
