package httpclient

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"github.com/tomdoesdev/dac/internal/application"
)

// stallGuard cancels a request when body reads stop making progress.
// Once the timer has fired, stalled stays set and every later error is reported as a stall, io.EOF included.
// The one-way flag cancels a request after the stall timer fires.
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
