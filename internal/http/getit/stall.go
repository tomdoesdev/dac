package getit

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// guardedBody is one response body, with the three things every request in this
// package needs around one: a stall timer, a byte count, and a byte limit.
//
// They live together because they all hang off the same Read. A body that
// delivers nothing for the stall timeout has its request cancelled. Bytes are
// counted as they arrive and reported to the hooks. And a transfer that passes
// its limit stops, whatever length the origin declared.
//
// Once the timer has fired, stalled stays set, and every later error is
// reported as a stall, io.EOF included: the read did not reach the end of the
// body, it reached the end of a cancelled request.
type guardedBody struct {
	body    io.ReadCloser
	cancel  context.CancelFunc
	timer   *time.Timer
	timeout time.Duration
	stalled atomic.Bool
	closed  atomic.Bool

	hooks    hookList
	transfer uint64
	part     int
	length   int64
	// received is the running byte count of the whole transfer, which every part
	// of a split download adds to.
	received *atomic.Int64
	// limit bounds the transfer. Zero is no limit.
	limit int64
}

// guard takes ownership of body, and of cancelling the request behind it.
func guard(body io.ReadCloser, cancel context.CancelFunc, part int, state *exchange) io.ReadCloser {
	guarded := &guardedBody{
		body:     body,
		cancel:   cancel,
		timeout:  state.settings.stallTimeout,
		hooks:    state.settings.hooks,
		transfer: state.transfer,
		part:     part,
		length:   state.length,
		received: state.received,
		limit:    state.settings.maxBytes,
	}
	if guarded.timeout > 0 {
		guarded.timer = time.AfterFunc(guarded.timeout, func() {
			guarded.stalled.Store(true)
			cancel()
		})
	}
	return guarded
}

func (guarded *guardedBody) Read(buffer []byte) (int, error) {
	count, err := guarded.body.Read(buffer)
	if count > 0 {
		// A closed body's timer has been stopped, and restarting it here would
		// cancel a context that nothing is watching any more.
		if guarded.timer != nil && !guarded.closed.Load() {
			guarded.timer.Reset(guarded.timeout)
		}
		total := guarded.received.Add(int64(count))
		guarded.hooks.progress(Progress{
			Transfer: guarded.transfer,
			Part:     guarded.part,
			Bytes:    int64(count),
			Received: total,
			Length:   guarded.length,
		})
		// The bytes already read are returned with the failure, because they did
		// arrive. What stops is everything after them.
		if guarded.limit > 0 && total > guarded.limit {
			return count, fmt.Errorf("%w of %d bytes", ErrTooLarge, guarded.limit)
		}
	}
	if err != nil && guarded.stalled.Load() {
		return count, fmt.Errorf("%w after %s", ErrStalled, guarded.timeout)
	}
	return count, err
}

func (guarded *guardedBody) Close() error {
	guarded.closed.Store(true)
	if guarded.timer != nil {
		guarded.timer.Stop()
	}
	err := guarded.body.Close()
	guarded.cancel()
	return err
}
