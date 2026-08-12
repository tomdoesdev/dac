package asset

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultMaxSize bounds an undeclared download while remaining large enough
	// for SDKs, archives, and other package-manager-style artifacts.
	DefaultMaxSize int64 = 4 * 1024 * 1024 * 1024
	// DefaultIdleTimeout stops a body read that makes no progress. It is not a
	// total transfer deadline, so active downloads may run for as long as needed.
	DefaultIdleTimeout = 30 * time.Second
)

var (
	// ErrInvalidMaxSize marks a transfer size outside dac's supported grammar.
	ErrInvalidMaxSize = errors.New("invalid maximum download size")
	// ErrInvalidIdleTimeout marks a negative or malformed idle duration.
	ErrInvalidIdleTimeout = errors.New("invalid download idle timeout")
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

// ParseMaxSize converts an integral human-readable byte size into bytes. Both
// decimal and IEC units are accepted; a bare value is already bytes.
func ParseMaxSize(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, ErrInvalidMaxSize
	}
	index := 0
	for index < len(trimmed) && trimmed[index] >= '0' && trimmed[index] <= '9' {
		index++
	}
	if index == 0 {
		return 0, ErrInvalidMaxSize
	}
	number, err := strconv.ParseUint(trimmed[:index], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidMaxSize, err)
	}
	multipliers := map[string]uint64{
		"": 1, "b": 1,
		"kb": 1000, "mb": 1000 * 1000, "gb": 1000 * 1000 * 1000, "tb": 1000 * 1000 * 1000 * 1000,
		"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
	}
	multiplier, ok := multipliers[strings.ToLower(trimmed[index:])]
	if !ok || number > math.MaxInt64/multiplier {
		return 0, ErrInvalidMaxSize
	}
	return int64(number * multiplier), nil
}

// ParseIdleTimeout accepts Go duration syntax and the special value zero.
func ParseIdleTimeout(value string) (time.Duration, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "0" {
		return 0, nil
	}
	duration, err := time.ParseDuration(trimmed)
	if err != nil || duration < 0 {
		return 0, ErrInvalidIdleTimeout
	}
	return duration, nil
}
