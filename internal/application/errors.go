package application

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"strings"

	"github.com/tomdoesdev/dac/internal/fault"
)

// ErrTooLarge marks a response that exceeded its size limit.
var ErrTooLarge = errors.New("asset is larger than the permitted size")

// ErrStalled marks a response that stopped transferring bytes.
var ErrStalled = errors.New("asset transfer stalled")

// transferError marks a failure that came from reading the asset body rather than from the object store.
// Installing an asset is one call that reads the network and writes the cache, and
// the two failures need different codes: a network that stopped delivering bytes is
// not a publisher that changed them.
type transferError struct{ Err error }

func (value *transferError) Error() string { return value.Err.Error() }

func (value *transferError) Unwrap() error { return value.Err }

// Unknown marks a ContentError size that DAC could not determine.
const Unknown = int64(-1)

// ContentError reports bytes that failed their digest or size check.
type ContentError struct {
	ExpectedDigest string
	ActualDigest   string
	ExpectedSize   int64
	ActualSize     int64
}

func (value *ContentError) Error() string {
	parts := make([]string, 0, 2)
	if value.ExpectedDigest != "" && value.ActualDigest != "" {
		parts = append(parts, fmt.Sprintf("digest is %s, not %s", value.ActualDigest, value.ExpectedDigest))
	}
	if value.ExpectedSize != Unknown {
		if value.ActualSize == Unknown {
			parts = append(parts, fmt.Sprintf("size is more than %d bytes", value.ExpectedSize))
		} else if value.ActualSize != value.ExpectedSize {
			parts = append(parts, fmt.Sprintf("size is %d, not %d", value.ActualSize, value.ExpectedSize))
		}
	}
	if len(parts) == 0 {
		return "content check failed"
	}
	return "content " + strings.Join(parts, " and ")
}

// Details returns the stable JSON details for one content failure.
func (value *ContentError) Details() map[string]any {
	details := map[string]any{}
	if value.ExpectedDigest != "" {
		details["expectedDigest"] = value.ExpectedDigest
	}
	if value.ActualDigest != "" {
		details["actualDigest"] = value.ActualDigest
	}
	if value.ExpectedSize != Unknown {
		details["expectedSize"] = value.ExpectedSize
	}
	if value.ActualSize != Unknown {
		details["actualSize"] = value.ActualSize
	}
	return details
}

// CorruptError reports a cache object whose bytes no longer hash to the digest that names it.
type CorruptError struct {
	Digest       string
	ActualDigest string
	Path         string
}

func (value *CorruptError) Error() string {
	return fmt.Sprintf("cache object contains %s, not %s", value.ActualDigest, value.Digest)
}

// Details returns the stable JSON details for one corrupt object.
func (value *CorruptError) Details() map[string]any {
	details := map[string]any{"expectedDigest": value.Digest}
	if value.ActualDigest != "" {
		details["actualDigest"] = value.ActualDigest
	}
	if value.Path != "" {
		details["path"] = value.Path
	}
	return details
}

// cacheReadError gives every cache lookup failure one code and one message, so the wording cannot drift between the eight places that report it.
func cacheReadError(err error) error {
	var corrupt *CorruptError
	if errors.As(err, &corrupt) {
		return &fault.Error{
			Code:    "cache_object_corrupt",
			Message: "The cache object does not match its digest. Run dac cache scrub --repair, then dac pull.",
			Details: corrupt.Details(),
			Cause:   err,
		}
	}
	return fault.Wrap("cache_read_failed", "DAC could not read the cache object.", err)
}

// corrupted reports whether an error describes a cache object that failed its integrity check.
func corrupted(err error) bool {
	var corrupt *CorruptError
	return errors.As(err, &corrupt)
}

// RequestDetail is implemented by a transport error that can name the request it failed.
// RequestDetail lets adapters report requests without an HTTP package dependency.
type RequestDetail interface {
	RequestURL() string
	// StatusCode reports the HTTP status, or zero when the request never received a response at all.
	StatusCode() int
}

func networkError(err error) error {
	code, message := "network_error", "The asset request failed."
	var network net.Error
	switch {
	case errors.Is(err, context.Canceled):
		code, message = "cancelled", "The command was cancelled."
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, ErrStalled):
		code, message = "timeout", "The network request timed out."
	case errors.As(err, &network) && network.Timeout():
		code, message = "timeout", "The network request timed out."
	}
	return &fault.Error{Code: code, Message: message, Details: requestDetails(err), Cause: err}
}

// requestDetails names the request behind a transport failure.
// Request details keep JSON consumers from parsing status and URL from error text.
func requestDetails(err error) map[string]any {
	details := map[string]any{}
	var detail RequestDetail
	if errors.As(err, &detail) {
		if value := detail.RequestURL(); value != "" {
			details["url"] = value
		}
		if value := detail.StatusCode(); value != 0 {
			details["status"] = value
		}
	}
	if len(details) == 0 {
		return nil
	}
	return details
}

// contentError turns a failed installation into a stable command error.
// Installing an asset reads the network and writes the cache in one call, so the
// three things that can go wrong there are reported as three different failures
// rather than all as a content check. Only bytes that arrived and then failed
// their check are a content mismatch; a transfer keeps the code its own failure
// has, which is the one that says whether to retry or to look at the publisher.
func contentError(err error) error {
	var transfer *transferError
	if errors.As(err, &transfer) {
		return networkError(transfer.Err)
	}
	if errors.Is(err, ErrTooLarge) {
		return fault.Wrap("asset_too_large", "The asset is larger than the permitted size.", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return networkError(err)
	}
	var mismatch *ContentError
	if errors.As(err, &mismatch) {
		// The message stays general and the cause carries the digests.
		return &fault.Error{
			Code:    "content_mismatch",
			Message: "The downloaded asset failed its content check.",
			Details: mismatch.Details(),
			Cause:   err,
		}
	}
	// Nothing is left that the network or the bytes could account for, so it came from the store: the temporary file, the hash, or the install.
	return fault.Wrap("cache_write_failed", "DAC could not write the asset to the cache.", err)
}

func withAsset(err error, name string) error {
	value := fault.As(err)
	details := maps.Clone(value.Details)
	if details == nil {
		details = map[string]any{}
	}
	details["asset"] = name
	message := strings.TrimSuffix(value.Message, ".")
	return &fault.Error{
		Code: value.Code,
		// The coordinate belongs in the text as well as JSON details: a parallel
		// operation otherwise leaves terminal users guessing which asset failed.
		Message: fmt.Sprintf("%s for %s.", message, name),
		Details: details,
		Cause:   value.Cause,
	}
}
