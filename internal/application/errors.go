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

// CorruptError reports a cache object whose bytes no longer hash to the digest
// that names it. The store raises it instead of returning the object, because a
// content-addressed name is a claim about content, and a store that hands back
// bytes failing that claim is worse than one that has nothing to hand back.
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

// cacheReadError gives every cache lookup failure one code and one message, so
// the wording cannot drift between the eight places that report it. A corrupt
// object gets its own code: it is the one cache failure an operator can act on,
// and it needs a different instruction from "the disk would not answer".
func cacheReadError(err error) error {
	var corrupt *CorruptError
	if errors.As(err, &corrupt) {
		return &fault.Error{
			Code:    "cache_object_corrupt",
			Message: "The cache object does not match its digest. Run dac cache verify --repair, then dac pull.",
			Details: corrupt.Details(),
			Cause:   err,
		}
	}
	return fault.Wrap("cache_read_failed", "DAC could not read the cache object.", err)
}

// corrupted reports whether an error describes a cache object that failed its
// integrity check.
func corrupted(err error) bool {
	var corrupt *CorruptError
	return errors.As(err, &corrupt)
}

// RequestDetail is implemented by a transport error that can name the request
// it failed.
//
// The interface lives here rather than beside the HTTP client because the
// dependency between the two points this way, and because a caller that only
// wants the URL and the status should not have to know which transport produced
// them.
type RequestDetail interface {
	RequestURL() string
	// StatusCode reports the HTTP status, or zero when the request never
	// received a response at all.
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
	case errors.Is(err, urlpolicy.ErrNotPermitted):
		code, message = "url_not_permitted", "The asset URL is not permitted."
	case errors.As(err, &network) && network.Timeout():
		code, message = "timeout", "The network request timed out."
	}
	return &fault.Error{Code: code, Message: message, Details: requestDetails(err), Cause: err}
}

// requestDetails names the request behind a transport failure.
//
// A JSON consumer gets the status and the URL as data rather than having to
// parse them back out of a sentence, which is the difference between a CI
// failure someone can read and one someone has to reproduce by hand.
func requestDetails(err error) map[string]any {
	var detail RequestDetail
	if !errors.As(err, &detail) {
		return nil
	}
	details := map[string]any{}
	if value := detail.RequestURL(); value != "" {
		details["url"] = value
	}
	if value := detail.StatusCode(); value != 0 {
		details["status"] = value
	}
	if len(details) == 0 {
		return nil
	}
	return details
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
		// The message stays general and the cause carries the digests. Writing
		// them into both produced a line that said the same thing twice.
		return &fault.Error{
			Code:    "content_mismatch",
			Message: "The downloaded asset failed its content check.",
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
