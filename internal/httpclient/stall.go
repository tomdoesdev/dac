package httpclient

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/tom/dac/internal/application"
)

// stallGuard cancels a request when body reads stop making progress.
//
// Once the timer has fired, stalled stays set and every later error is reported
// as a stall, io.EOF included. Both halves of that are deliberate, and both look
// like bugs from close up.
//
// The flag is one way because what it pairs with is: the timer cancels the
// request, so nothing after it is going to succeed, and there is no state to
// return to. Converting io.EOF matters more. A stall that truncates a body can
// surface as a clean end of stream, and for an asset the manifest does not pin
// there is no digest waiting to catch it -- DAC would lock the bytes it got.
// Exempting io.EOF trades a rare false failure, on a transfer that finished in
// the same instant the timer fired, for a rare silent wrong answer. A retry
// costs a download; a lock file naming half an asset costs whatever runs next.
type stallGuard struct {
	body    io.ReadCloser
	cancel  context.CancelFunc
	timer   *time.Timer
	timeout time.Duration
	stalled atomic.Bool
	closed  atomic.Bool
}

func guard(body io.ReadCloser, cancel context.CancelFunc, timeout time.Duration) io.ReadCloser {
	guarded := &stallGuard{body: body, cancel: cancel, timeout: timeout}
	if timeout > 0 {
		guarded.timer = time.AfterFunc(timeout, func() {
			guarded.stalled.Store(true)
			cancel()
		})
	}
	return guarded
}

func (guarded *stallGuard) Read(buffer []byte) (int, error) {
	count, err := guarded.body.Read(buffer)
	if count > 0 && guarded.timer != nil && !guarded.closed.Load() {
		guarded.timer.Reset(guarded.timeout)
	}
	if err != nil && guarded.stalled.Load() {
		return count, fmt.Errorf("%w after %s", application.ErrStalled, guarded.timeout)
	}
	return count, err
}

func (guarded *stallGuard) Close() error {
	guarded.closed.Store(true)
	if guarded.timer != nil {
		guarded.timer.Stop()
	}
	err := guarded.body.Close()
	guarded.cancel()
	return err
}
