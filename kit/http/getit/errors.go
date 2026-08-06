package getit

import (
	"errors"
	"fmt"
	"time"
)

// ErrStalled marks a body that stopped delivering bytes.
// A request that stalls is cancelled rather than left to wait, because a
// connection that has gone quiet does not report that it has gone quiet.
var ErrStalled = errors.New("transfer stalled")

// ErrPermanent marks a failure that another attempt cannot change.
//
// It is the contract between this package and a transport decorator. A
// decorator that refuses a request wraps its refusal with Permanent, and the
// refusal is then made once instead of once for every retry of every range of a
// split download.
var ErrPermanent = errors.New("request refused")

// ErrTooLarge marks a body that passed the byte limit the request set.
var ErrTooLarge = errors.New("response is larger than the limit")

// ErrNotPermitted marks a URL that the URL policy refused.
// The policy applies to the URL of the request and to the target of every
// redirect it follows.
var ErrNotPermitted = errors.New("URL is not permitted")

// Permanent marks an error as one that another attempt cannot change.
// A transport decorator returns this for a request it refuses.
//
// The result matches ErrPermanent and the error it was given, so a caller can
// still find its own error with errors.Is or errors.As.
func Permanent(err error) error {
	if err == nil || errors.Is(err, ErrPermanent) {
		return err
	}
	return &permanentError{err: err}
}

type permanentError struct{ err error }

func (value *permanentError) Error() string { return value.err.Error() }

// Unwrap reports both errors, so this matches the error it wraps and ErrPermanent.
func (value *permanentError) Unwrap() []error { return []error{value.err, ErrPermanent} }

// RequestError names the request behind a failure.
// The URL and the status are added at the boundary of the package, after the
// retry rule has looked at the error under them.
type RequestError struct {
	// URL is the request this failure belongs to.
	URL string
	// Status is the HTTP status, or zero when the request never got a response.
	Status int
	Err    error
}

func (value *RequestError) Error() string { return fmt.Sprintf("%s: %v", value.URL, value.Err) }

func (value *RequestError) Unwrap() error { return value.Err }

// StatusError reports a status that the request cannot use.
type StatusError struct {
	// Kind names what was asked for: "request", "conditional request", or "range".
	Kind string
	// Status is the HTTP status the origin answered with.
	Status int
	// RetryAfter is the wait a Retry-After header asked for, or zero for none.
	RetryAfter time.Duration
}

func (value *StatusError) Error() string {
	return fmt.Sprintf("%s returned HTTP %d", value.Kind, value.Status)
}

// statusOf reports the HTTP status behind an error, or zero when it has none.
func statusOf(err error) int {
	var status *StatusError
	if errors.As(err, &status) {
		return status.Status
	}
	return 0
}
