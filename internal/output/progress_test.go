package output

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tomdoesdev/kit/cli"
)

// TestDownloadGroupAnnouncesEveryConcurrentDownloadOnItsOwnLine covers the
// contract that makes concurrent transfers presentable: one progress line
// describes the batch, each finished download leaves a complete record of its
// own, and no record is interleaved with the animation sharing that line.
func TestDownloadGroupAnnouncesEveryConcurrentDownloadOnItsOwnLine(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	writer := New(&Options{}, stdout, stderr, WithThrobber(cli.ThrobberAlways, time.Hour))
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
				announced, runErr := group.Run(ctx, item, func(context.Context) error { return nil })
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
	if !strings.Contains(progress, "Downloading 3 files…") {
		t.Fatalf("batch progress line missing: %q", progress)
	}
	for _, item := range pending {
		if !reported[item.File] {
			t.Fatalf("%s did not report that it announced itself", item.File)
		}
		record := "\r\x1b[2K✔ " + item.File + " downloaded from "
		if !strings.Contains(progress, record) {
			t.Fatalf("record for %s missing or not on a cleared line: %q", item.File, progress)
		}
	}
	if strings.Contains(progress, "private") || strings.Contains(progress, "secret") {
		t.Fatalf("progress leaked source details: %q", progress)
	}
	// A record always ends its own line, so a frame drawn beside it can never
	// be mistaken for part of a permanent one.
	for _, line := range strings.Split(progress, "\n") {
		if strings.Count(line, "✔") > 1 {
			t.Fatalf("two records shared one line: %q", line)
		}
	}
}

// TestFailedConcurrentDownloadAnnouncesNothing keeps a batch honest about what
// arrived: a transfer that failed leaves no record, while the ones beside it
// keep theirs.
func TestFailedConcurrentDownloadAnnouncesNothing(t *testing.T) {
	stderr := &bytes.Buffer{}
	writer := New(&Options{}, &bytes.Buffer{}, stderr, WithThrobber(cli.ThrobberAlways, time.Hour))
	pending := []Download{
		{File: "good.bin", URL: "https://example.com/good.bin"},
		{File: "bad.bin", URL: "https://example.com/bad.bin"},
	}
	want := errors.New("download failed")
	err := writer.WithDownloads(context.Background(), pending, func(ctx context.Context, group *DownloadGroup) error {
		if _, err := group.Run(ctx, pending[0], func(context.Context) error { return nil }); err != nil {
			return err
		}
		reported, err := group.Run(ctx, pending[1], func(context.Context) error { return want })
		if reported {
			t.Error("a failed download claimed to have announced itself")
		}
		return err
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	progress := stderr.String()
	if !strings.Contains(progress, "✔ good.bin") || strings.Contains(progress, "bad.bin downloaded") {
		t.Fatalf("stderr = %q", progress)
	}
	if !strings.HasSuffix(progress, "\r\x1b[2K") {
		t.Fatalf("progress line survived a failed batch: %q", progress)
	}
}

// TestEmptyBatchPresentsNothing keeps a command that found everything locally
// from announcing work it is not doing.
func TestEmptyBatchPresentsNothing(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	writer := New(&Options{}, stdout, stderr, WithThrobber(cli.ThrobberAlways, time.Hour))
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
