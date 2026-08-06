package getit

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// The bounds on how long a retry waits.
const (
	firstBackoff = 250 * time.Millisecond
	limitBackoff = 8 * time.Second
	// limitRetryAfter caps what this package will wait because an origin asked
	// it to. An origin that asks for an hour is asking for more than a caller
	// with a context and a timeout of its own is willing to give.
	limitRetryAfter = 30 * time.Second
)

// permanent names the failures that asking again cannot change.
var permanent = []error{
	context.Canceled,
	context.DeadlineExceeded,
	ErrPermanent,
	ErrNotPermitted,
	ErrTooLarge,
}

// Retryable reports whether a failure is worth another attempt.
// It is the rule this package applies when a caller names no other, and a
// caller that replaces it with WithRetryPolicy can still call it.
//
// The answer defaults to yes, because a transport error this package does not
// recognize is more often a network that dropped than a request that nobody
// will ever answer.
func Retryable(err error) bool {
	var status *StatusError
	if errors.As(err, &status) {
		return status.Status == http.StatusTooManyRequests ||
			status.Status == http.StatusRequestTimeout ||
			status.Status >= 500 && status.Status <= 599
	}
	for _, sentinel := range permanent {
		if errors.Is(err, sentinel) {
			return false
		}
	}
	return true
}

// retry applies one rule to whole requests and to range requests.
// The attempt number is passed in so that what reports a request can say which
// try it is. It counts from 1.
func retry[T any](
	ctx context.Context,
	retries int,
	policy func(error) bool,
	operation func(attempt int) (T, error),
	notify func(attempt int, err error, wait time.Duration),
) (T, error) {
	for attempt := 1; ; attempt++ {
		value, err := operation(attempt)
		if err == nil {
			return value, nil
		}
		if attempt > retries || !policy(err) || ctx.Err() != nil {
			var zero T
			return zero, err
		}
		wait := backoff(attempt, err)
		if notify != nil {
			notify(attempt, err, wait)
		}
		if err := sleep(ctx, wait); err != nil {
			var zero T
			return zero, err
		}
	}
}

// backoff is how long to wait before attempt number attempt is tried again.
// An origin that named a wait gets the wait it named. Otherwise the wait
// doubles each time, with a random part added so that a set of transfers that
// failed together does not come back all at once.
func backoff(attempt int, err error) time.Duration {
	var status *StatusError
	if errors.As(err, &status) && status.RetryAfter > 0 {
		return min(status.RetryAfter, limitRetryAfter)
	}
	wait := min(time.Duration(1<<(attempt-1))*firstBackoff, limitBackoff)
	return wait + rand.N(wait/2+time.Millisecond)
}

// retryAfter reads the wait a Retry-After header asks for, in either of the two
// forms it is written in: a count of seconds, or a date.
func retryAfter(response *http.Response) time.Duration {
	value := strings.TrimSpace(response.Header.Get("Retry-After"))
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Until(when), 0)
	}
	return 0
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
