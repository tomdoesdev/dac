package getit

import (
	"net/url"
	"time"
)

// Unknown marks a length that the origin did not declare.
const Unknown = int64(-1)

// Hooks reports what one transfer is doing, as it does it.
//
// It is a struct of functions rather than an interface so that an
// implementation carries only the callbacks it wants, and so that a field added
// later breaks nobody. A nil field is not called.
//
// Three rules apply to every callback:
//
// A callback runs on whichever goroutine is doing the work. For a split
// download that is several goroutines at once, so an implementation must be
// safe for concurrent use.
//
// A callback must not block. The transfer waits behind it.
//
// A callback must not touch the transfer it is reporting on. It is called from
// inside the read it describes.
type Hooks struct {
	// OnStart reports one transfer, once, as soon as its shape is known: after
	// the first response has been checked and before Get returns.
	OnStart func(Transfer)
	// OnRequest reports each request before it goes out. That is the first
	// request, each range of a split download, and each retry of either.
	OnRequest func(Request)
	// OnResponse reports the status each request answered with, or the transport
	// error it failed with and a status of zero.
	OnResponse func(Request, int, error)
	// OnProgress reports body bytes as they arrive.
	OnProgress func(Progress)
	// OnRetry reports a request that failed, and the wait before it is tried again.
	OnRetry func(Request, error, time.Duration)
	// OnEnd reports one transfer, once, when its body is closed.
	//
	// It pairs with OnStart: both fire, or neither does. A Get that failed
	// before it had a response to return fires neither, because the failure is
	// what Get returned and there is nothing a report could add to it.
	//
	// The error is what ended the transfer, or nil when nothing failed. A caller
	// that closes a body without reading to the end also ends with nil: nothing
	// failed, the caller stopped. Close the body of every response, including a
	// not-modified one, or this is never called.
	OnEnd func(Transfer, error)
}

// Transfer is one Get call, across every request it makes.
type Transfer struct {
	// ID is unique among the transfers of one Getter.
	ID uint64
	// URL is the URL as it was requested, before any redirect.
	URL *url.URL
	// Length is the size of the body, or Unknown when the origin declared none.
	Length int64
	// Parts is how many requests carry the body: one, or the chunk count of a
	// split download. A not-modified response carries no body, and reports zero.
	Parts int
}

// Request is one HTTP request that a transfer makes.
type Request struct {
	// Transfer is the ID of the transfer this request belongs to.
	Transfer uint64
	// Part is 0 for the first request and 1 upwards for each range after it.
	Part int
	// Attempt is 1 for the first try of this request.
	Attempt int
	Method  string
	URL     *url.URL
	// Start and End are the first and last byte this request asks for, or
	// Unknown and Unknown when it asks for the whole body.
	Start, End int64
}

// Progress is body bytes that have arrived.
//
// The bytes are counted as they are received from the network, not as a caller
// reads them. In a split download the two differ, because a range that arrives
// early waits in memory for its turn. Received is what a download meter should
// show. A caller that wants the bytes it has read instead counts them in a
// wrapper around Response.Body.
type Progress struct {
	// Transfer is the ID of the transfer these bytes belong to.
	Transfer uint64
	// Part is the request that carried them: 0 for the first, 1 upwards for a range.
	Part int
	// Bytes is how many arrived in this report. Summing this over the reports of
	// one transfer is exact, whatever order they arrive in.
	Bytes int64
	// Received is the total for the whole transfer once these bytes were added
	// to it.
	//
	// Parts count their bytes without waiting for each other, so that a slow
	// callback holds up one part rather than all of them. The cost is that two
	// reports can arrive in the other order from the one they were counted in,
	// and a later report can then carry a smaller Received. A meter that needs a
	// total that only grows adds Bytes up itself, or takes the largest Received
	// it has seen. The largest is always the whole transfer.
	Received int64
	// Length is the size of the body, or Unknown when the origin declared none.
	Length int64
}

// hookList is the hooks of a Getter and of one Get call, together.
// Both fire, in the order they were added.
type hookList []Hooks

func (list hookList) start(transfer Transfer) {
	for _, hooks := range list {
		if hooks.OnStart != nil {
			hooks.OnStart(transfer)
		}
	}
}

func (list hookList) request(request Request) {
	for _, hooks := range list {
		if hooks.OnRequest != nil {
			hooks.OnRequest(request)
		}
	}
}

func (list hookList) response(request Request, status int, err error) {
	for _, hooks := range list {
		if hooks.OnResponse != nil {
			hooks.OnResponse(request, status, err)
		}
	}
}

func (list hookList) progress(progress Progress) {
	for _, hooks := range list {
		if hooks.OnProgress != nil {
			hooks.OnProgress(progress)
		}
	}
}

func (list hookList) retry(request Request, err error, wait time.Duration) {
	for _, hooks := range list {
		if hooks.OnRetry != nil {
			hooks.OnRetry(request, err, wait)
		}
	}
}

func (list hookList) end(transfer Transfer, err error) {
	for _, hooks := range list {
		if hooks.OnEnd != nil {
			hooks.OnEnd(transfer, err)
		}
	}
}
