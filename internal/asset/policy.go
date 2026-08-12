package asset

import "time"

const (
	// DefaultMaxSize bounds an undeclared download while remaining large enough
	// for SDKs, archives, and other package-manager-style artifacts.
	DefaultMaxSize int64 = 4 * 1024 * 1024 * 1024
	// DefaultIdleTimeout stops a body read that makes no progress. It is not a
	// total transfer deadline, so active downloads may run for as long as needed.
	DefaultIdleTimeout = 30 * time.Second
	// DefaultDownloadJobs is how many assets dac transfers at once when an
	// invocation does not say. A handful of transfers is enough to keep a link
	// busy while one of them waits on a server, and small enough that dac stays
	// an unremarkable client of the hosts it fetches from.
	DefaultDownloadJobs = 4
	// MaxDownloadJobs bounds what an invocation may ask for. dac downloads whole
	// artifacts rather than many small files, so past a certain point extra
	// transfers only divide the same bandwidth into more ways to time out, and
	// the connection pool below is sized for this many at once.
	MaxDownloadJobs = 16
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
