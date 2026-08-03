package application

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"strings"

	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/urlpolicy"
)

// ErrTooLarge marks a response that exceeded its size limit.
var ErrTooLarge = errors.New("asset is larger than the permitted size")

// ErrStalled marks a response that stopped transferring bytes.
var ErrStalled = errors.New("asset transfer stalled")

// Unknown marks a ContentError size that DAC could not determine.
const Unknown = int64(-1)

// ContentError reports bytes that failed their digest or size check. It carries
// what DAC actually received so an operator can compare it against the expected
// value without downloading the asset again by hand.
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

// cacheReadError gives every cache lookup failure one code and one message, so
// the wording cannot drift between the eight places that report it.
func cacheReadError(err error) error {
	return fault.Wrap("cache_read_failed", "DAC could not read the cache object.", err)
}

func networkError(err error) error {
	if errors.Is(err, context.Canceled) {
		return fault.Wrap("cancelled", "The command was cancelled.", err)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrStalled) {
		return fault.Wrap("timeout", "The network request timed out.", err)
	}
	var network net.Error
	if errors.As(err, &network) && network.Timeout() {
		return fault.Wrap("timeout", "The network request timed out.", err)
	}
	if errors.Is(err, urlpolicy.ErrNotPermitted) {
		return fault.Wrap("url_not_permitted", "The asset URL is not permitted.", err)
	}
	return fault.Wrap("network_error", "The asset request failed.", err)
}

// contentError turns a failed content check into a stable command error. It
// keeps the digest DAC actually received in both the human message and the JSON
// details: without it an operator cannot tell a corrupted transfer apart from a
// publisher who replaced the bytes behind a stable URL.
func contentError(err error) error {
	if errors.Is(err, ErrTooLarge) {
		return fault.Wrap("asset_too_large", "The asset is larger than the permitted size.", err)
	}
	if errors.Is(err, context.Canceled) {
		return fault.Wrap("cancelled", "The command was cancelled.", err)
	}
	var mismatch *ContentError
	if errors.As(err, &mismatch) {
		return &fault.Error{
			Code:    "content_mismatch",
			Message: "The downloaded asset failed its content check: " + mismatch.Error() + ".",
			Details: mismatch.Details(),
			Cause:   err,
		}
	}
	return fault.Wrap("content_mismatch", "The downloaded asset failed its content check.", err)
}

func withAsset(err error, name string) error {
	value := fault.As(err)
	details := maps.Clone(value.Details)
	if details == nil {
		details = map[string]any{}
	}
	details["asset"] = name
	return &fault.Error{Code: value.Code, Message: value.Message, Details: details, Cause: value.Cause}
}
