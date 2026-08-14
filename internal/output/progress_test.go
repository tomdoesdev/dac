package output

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestDownloadGroupAnnouncesEveryConcurrentDownloadOnItsOwnLine covers the
// contract that makes concurrent transfers presentable: mpb gives every active
// transfer its own bar and leaves each completion on a distinct line.
func TestDownloadGroupAnnouncesEveryConcurrentDownloadOnItsOwnLine(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	writer := New(&Options{}, stdout, stderr, withProgress(progressAlways, time.Millisecond))
	pending := []Download{
		{File: "first.bin", URL: "https://first.example.com/private/first.bin?token=secret"},
		{File: "second.bin", URL: "https://second.example.com/second.bin"},
		{File: "third.bin", URL: "https://third.example.com/third.bin"},
	}

	var mutex sync.Mutex
	reported := make(map[string]bool, len(pending))
	err := writer.WithDownloads(context.Background(), pending, func(ctx context.Context, group *DownloadGroup) error {
		var transfers sync.WaitGroup
		for _, item := range pending {
			transfers.Add(1)
			go func() {
				defer transfers.Done()
				announced, runErr := group.Run(ctx, item, func(_ context.Context, progress DownloadProgress) error {
					progress(1024, 2048)
					time.Sleep(10 * time.Millisecond)
					progress(2048, 2048)
					return nil
				})
				mutex.Lock()
				defer mutex.Unlock()
				reported[item.File] = announced && runErr == nil
			}()
		}
		transfers.Wait()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	progress := stderr.String()
	for _, item := range pending {
		if !reported[item.File] {
			t.Fatalf("%s did not report that it announced itself", item.File)
		}
		record := "✔  " + item.File
		if !strings.Contains(progress, record) {
			t.Fatalf("record for %s missing: %q", item.File, progress)
		}
	}
	if strings.Contains(progress, "private") || strings.Contains(progress, "secret") {
		t.Fatalf("progress leaked source details: %q", progress)
	}
	type columns struct {
		file, host, bar, sizeSeparator, percentage int
	}
	var aligned *columns
	for _, item := range pending {
		var completedLine string
		for _, line := range strings.Split(progress, "\n") {
			if strings.Contains(line, "✔  "+item.File) {
				completedLine = line
				break
			}
		}
		if completedLine == "" {
			t.Fatalf("completed row for %s missing: %q", item.File, progress)
		}
		positions := columns{
			file:          strings.Index(completedLine, item.File),
			host:          strings.Index(completedLine, downloadHostname(item.URL)),
			bar:           strings.Index(completedLine, "["),
			sizeSeparator: strings.Index(completedLine, "/"),
			percentage:    strings.Index(completedLine, "%"),
		}
		if aligned == nil {
			aligned = &positions
			continue
		}
		if positions != *aligned {
			t.Fatalf("columns for %s = %+v, want %+v in %q", item.File, positions, *aligned, completedLine)
		}
	}
	// A record always ends its own line, so a frame drawn beside it can never
	// be mistaken for part of a permanent one.
	for _, line := range strings.Split(progress, "\n") {
		if strings.Count(line, "✔") > 1 {
			t.Fatalf("two records shared one line: %q", line)
		}
	}
}

// TestFailedConcurrentDownloadDoesNotAnnounceSuccess keeps a batch honest about each
// transfer: a completed peer may retain its record, but the failed bar must not
// be converted into a success by mpb's terminal abort state.
func TestFailedConcurrentDownloadDoesNotAnnounceSuccess(t *testing.T) {
	stderr := &bytes.Buffer{}
	writer := New(&Options{}, &bytes.Buffer{}, stderr, withProgress(progressAlways, time.Millisecond))
	pending := []Download{
		{File: "good.bin", URL: "https://example.com/good.bin"},
		{File: "bad.bin", URL: "https://example.com/bad.bin"},
	}
	want := errors.New("download failed")
	err := writer.WithDownloads(context.Background(), pending, func(ctx context.Context, group *DownloadGroup) error {
		if _, err := group.Run(ctx, pending[0], func(_ context.Context, progress DownloadProgress) error {
			progress(4, 4)
			return nil
		}); err != nil {
			return err
		}
		reported, err := group.Run(ctx, pending[1], func(context.Context, DownloadProgress) error { return want })
		if reported {
			t.Error("a failed download claimed to have announced itself")
		}
		return err
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	progress := stderr.String()
	// The completed peer may or may not have reached a refresh before the
	// batch shuts down; only the failed transfer's absence is deterministic.
	if strings.Contains(progress, "✔  bad.bin") {
		t.Fatalf("stderr = %q", progress)
	}
}

// TestDownloadGroupCancellationStopsOperationAndRenderer verifies that the
// same context controls both sides of an interactive transfer. Returning from
// WithDownloads means mpb has joined its renderer goroutines, so a signal-
// derived cancellation cannot leave terminal activity running in the process.
func TestDownloadGroupCancellationStopsOperationAndRenderer(t *testing.T) {
	stderr := &bytes.Buffer{}
	writer := New(&Options{}, &bytes.Buffer{}, stderr, withProgress(progressAlways, time.Millisecond))
	item := Download{File: "slow.bin", URL: "https://example.com/slow.bin"}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	returned := make(chan error, 1)

	go func() {
		returned <- writer.WithDownloads(ctx, []Download{item}, func(ctx context.Context, group *DownloadGroup) error {
			_, err := group.Run(ctx, item, func(ctx context.Context, progress DownloadProgress) error {
				progress(0, 100)
				close(started)
				<-ctx.Done()
				return ctx.Err()
			})
			return err
		})
	}()

	<-started
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("download or progress renderer survived cancellation")
	}
	if strings.Contains(stderr.String(), "✔") {
		t.Fatalf("cancelled download reported success: %q", stderr.String())
	}
}

// TestEmptyBatchPresentsNothing keeps a command that found everything locally
// from announcing work it is not doing.
func TestEmptyBatchPresentsNothing(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	writer := New(&Options{}, stdout, stderr, withProgress(progressAlways, time.Millisecond))
	ran := false
	if err := writer.WithDownloads(context.Background(), nil, func(context.Context, *DownloadGroup) error {
		ran = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("an empty batch did not run its operation")
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("empty batch wrote stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
