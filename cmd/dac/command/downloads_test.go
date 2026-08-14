package command

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/tomdoesdev/dac/internal/asset"
)

// TestDownloadEachRespectsItsLimit keeps the concurrency option meaningful in
// both directions: a limit of one is a promise that a host sees one request at
// a time, and a higher limit is only a ceiling, never a number of transfers dac
// starts regardless of how much work there is.
func TestDownloadEachRespectsItsLimit(t *testing.T) {
	for _, limit := range []int{1, 2, 8} {
		items := make([]int, 4)
		expected := int64(min(limit, len(items)))
		var inFlight, peak, arrived atomic.Int64
		// Every transfer that may run at once must be running before any of them
		// returns, or the observed peak would only prove scheduling luck.
		ready := make(chan struct{})
		err := downloadEach(context.Background(), limit, items, func(context.Context, *int) error {
			current := inFlight.Add(1)
			defer inFlight.Add(-1)
			for highest := peak.Load(); current > highest; highest = peak.Load() {
				peak.CompareAndSwap(highest, current)
			}
			if arrived.Add(1) == expected {
				close(ready)
			}
			<-ready
			return nil
		})
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if want := int64(min(limit, len(items))); peak.Load() != want {
			t.Fatalf("limit %d ran %d at once, want %d", limit, peak.Load(), want)
		}
	}
}

// TestDownloadEachReportsTheFailureThatStoppedIt covers what a caller sees when
// one asset fails: the failure that caused the stop rather than the
// cancellations it produced, and every remaining transfer abandoned.
func TestDownloadEachReportsTheFailureThatStoppedIt(t *testing.T) {
	want := errors.New("upstream refused")
	items := make([]int, 32)
	var started atomic.Int64
	err := downloadEach(context.Background(), 2, items, func(ctx context.Context, _ *int) error {
		if started.Add(1) == 1 {
			return want
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if started.Load() >= int64(len(items)) {
		t.Fatalf("a failure did not stop the batch: %d of %d transfers started", started.Load(), len(items))
	}
}

// TestDownloadEachReportsParentCancellationBeforeDispatch covers the narrow
// signal race before the first HTTP request starts. The command must still fail
// as cancelled, rather than returning success because no worker produced an
// error of its own.
func TestDownloadEachReportsParentCancellationBeforeDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := downloadEach(ctx, 2, []int{1, 2}, func(context.Context, *int) error {
		called = true
		return nil
	})
	if called {
		t.Fatal("a transfer started after parent cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
}

// TestDownloadEachHandsEachTransferItsOwnItem pins the contract the commands
// depend on to assemble ordered results without locks: a transfer records its
// outcome on the element it was given, in the caller's slice.
func TestDownloadEachHandsEachTransferItsOwnItem(t *testing.T) {
	items := []int{0, 1, 2, 3, 4}
	if err := downloadEach(context.Background(), 3, items, func(_ context.Context, item *int) error {
		*item *= 10
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for index, value := range items {
		if value != index*10 {
			t.Fatalf("items = %v, want each element updated in place", items)
		}
	}
	if err := downloadEach(context.Background(), 3, nil, func(context.Context, *int) error {
		t.Fatal("an empty batch started a transfer")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestJobsValueDefaultsWithoutAcceptingZero keeps the unset state distinct from
// a number: dac's default is what an invocation gets when it says nothing, not
// something --jobs 0 can ask for.
func TestJobsValueDefaultsWithoutAcceptingZero(t *testing.T) {
	unset := &jobsValue{}
	if unset.limit() != asset.DefaultDownloadJobs {
		t.Fatalf("unset limit = %d, want %d", unset.limit(), asset.DefaultDownloadJobs)
	}
	for _, input := range []string{"0", "-1", "", "two", "1.5"} {
		if err := (&jobsValue{}).Set(input); !errors.Is(err, ErrInvalidJobs) {
			t.Fatalf("--jobs %q error = %v, want ErrInvalidJobs", input, err)
		}
	}
	if err := (&jobsValue{}).Set(strconv.Itoa(asset.MaxDownloadJobs + 1)); !errors.Is(err, ErrInvalidJobs) {
		t.Fatal("a concurrency above the maximum was accepted")
	}
	value := &jobsValue{}
	if err := value.Set("3"); err != nil || value.limit() != 3 {
		t.Fatalf("--jobs 3 = %d, %v", value.limit(), err)
	}
	if err := value.Set("4"); !errors.Is(err, ErrJobsRepeated) {
		t.Fatalf("repeated --jobs error = %v, want ErrJobsRepeated", err)
	}
}
