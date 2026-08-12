package command

import (
	"context"
	"strconv"
	"sync"

	"github.com/tomdoesdev/dac/internal/asset"
)

// jobsValue is the concurrency option shared by the commands that download.
// Its unset state means dac's default rather than a number, so an explicit
// --jobs 0 is rejected instead of quietly meaning something the flag does not
// say. Like dac's other scalar options it also refuses to be repeated.
type jobsValue struct {
	value int
	set   bool
}

func (value *jobsValue) String() string {
	if !value.set {
		return strconv.Itoa(asset.DefaultDownloadJobs)
	}
	return strconv.Itoa(value.value)
}

func (*jobsValue) Type() string { return "int" }

func (value *jobsValue) Set(input string) error {
	if value.set {
		return ErrJobsRepeated
	}
	parsed, err := strconv.Atoi(input)
	if err != nil || parsed < 1 || parsed > asset.MaxDownloadJobs {
		return ErrInvalidJobs
	}
	value.value, value.set = parsed, true
	return nil
}

// limit reports how many transfers this invocation permits at once.
func (value *jobsValue) limit() int {
	if !value.set {
		return asset.DefaultDownloadJobs
	}
	return value.value
}

// downloadEach runs one transfer per pending item, at most limit of them at a
// time, and stops the rest as soon as one fails. Each transfer is handed the
// element it is responsible for, which is how it records its outcome without
// sharing anything with the transfers beside it.
//
// The concurrency is between assets and never inside one: each transfer has its
// own request and its own staging file. The only ordering a caller gives up is
// the order transfers finish in, which nothing depends on — results, lock
// entries, and errors are all still reported in manifest order.
func downloadEach[T any](ctx context.Context, limit int, items []T, download func(context.Context, *T) error) error {
	if len(items) == 0 {
		return nil
	}
	limit = min(max(limit, 1), len(items))

	// The first failure cancels the transfers still running: their bytes can no
	// longer be accepted, and dac should stop asking hosts to keep sending them.
	batch, cancel := context.WithCancel(ctx)
	defer cancel()

	work := make(chan *T)
	var transfers sync.WaitGroup
	var once sync.Once
	var failure error
	for range limit {
		transfers.Add(1)
		go func() {
			defer transfers.Done()
			for item := range work {
				if err := download(batch, item); err != nil {
					// The first failure is the one that explains the run. Every
					// later error is usually the cancellation it caused, and
					// reporting that instead would hide the real cause.
					once.Do(func() {
						failure = err
						cancel()
					})
				}
			}
		}()
	}
	for index := range items {
		if batch.Err() != nil {
			break
		}
		work <- &items[index]
	}
	close(work)
	transfers.Wait()
	return failure
}
