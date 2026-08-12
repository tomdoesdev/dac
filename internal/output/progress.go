package output

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"sync"
	"time"

	"github.com/tomdoesdev/kit/cli"
)

// Download identifies one artifact for progress presentation. It carries only
// what a terminal line may show, so presentation never reaches into transfer
// or lock state to describe a download.
type Download struct {
	File string
	URL  string
}

// DownloadGroup presents a batch of downloads that may run at the same time.
// One progress line describes the batch while it runs, and each transfer
// leaves its own permanent record as it finishes.
//
// A frame of the animation and a finished download's record are written by
// different goroutines onto the same terminal line, so the group owns the
// writer both use and serializes every write onto it.
type DownloadGroup struct {
	writer   *Writer
	output   io.Writer
	animated bool
}

// WithDownloads runs operation while presenting pending as one batch. An empty
// batch presents nothing: a command whose files are all already local still
// calls this rather than deciding for itself what no progress looks like.
func (writer *Writer) WithDownloads(ctx context.Context, pending []Download, operation func(context.Context, *DownloadGroup) error) error {
	mode := writer.throbberMode
	if writer.options.JSON || writer.options.Quiet {
		mode = cli.ThrobberNever
	}
	group := &DownloadGroup{
		writer: writer,
		output: newExclusiveWriter(writer.stderr),
		// The throbber decides this for itself, but a record written while a
		// frame is on screen has to erase that frame first, which is only safe
		// when the animation is running at all.
		animated: cli.Animates(writer.stderr, mode),
	}
	if len(pending) == 0 {
		return operation(ctx, group)
	}
	throbber := cli.NewThrobber(
		group.output,
		batchMessage(pending),
		cli.WithThrobberMode(mode),
		cli.WithThrobberInterval(writer.throbberInterval),
		cli.WithThrobberStyle(writer.stderrStyler.Progress),
	)
	return throbber.Run(ctx, func(ctx context.Context) error { return operation(ctx, group) })
}

// Run performs one of the batch's downloads and announces it once it succeeds.
// It reports whether it announced the download, which the caller records on the
// Result so the summary does not repeat it. Run is safe to call concurrently,
// which is the whole reason the group owns the shared line.
func (group *DownloadGroup) Run(ctx context.Context, item Download, operation func(context.Context) error) (bool, error) {
	started := time.Now()
	if err := operation(ctx); err != nil {
		return false, err
	}
	if !group.animated {
		return false, nil
	}
	if err := group.announce(item, time.Since(started)); err != nil {
		return false, err
	}
	return true, nil
}

// announce leaves a permanent record of one finished download above the shared
// progress line. It erases the frame currently occupying that line so the
// record starts on a clean one, and the next frame redraws below it.
func (group *DownloadGroup) announce(item Download, elapsed time.Duration) error {
	styler := group.writer.stderrStyler
	record := fmt.Sprintf("\r\x1b[2K%s %s %s from %s (%s)\n",
		styler.Success("✔"),
		item.File,
		styler.Success("downloaded"),
		downloadHostname(item.URL),
		formatElapsed(elapsed))
	_, err := io.WriteString(group.output, record)
	return err
}

// WithDownloadProgress presents one download on its own. A single asset is the
// batch of one, so an invocation that downloads nothing concurrently still
// shares the presentation with the one that does.
func (writer *Writer) WithDownloadProgress(ctx context.Context, filename, sourceURL string, operation func(context.Context) error) (bool, error) {
	item := Download{File: filename, URL: sourceURL}
	var reported bool
	err := writer.WithDownloads(ctx, []Download{item}, func(ctx context.Context, group *DownloadGroup) error {
		var runErr error
		reported, runErr = group.Run(ctx, item, operation)
		return runErr
	})
	return reported, err
}

// batchMessage describes what the batch is waiting for. One download names
// itself and its host, which several cannot do on a single line, and which the
// record each of them leaves behind reports anyway.
func batchMessage(pending []Download) string {
	if len(pending) == 1 {
		return fmt.Sprintf("Downloading %s from %s…", pending[0].File, downloadHostname(pending[0].URL))
	}
	return fmt.Sprintf("Downloading %d files…", len(pending))
}

// downloadHostname deliberately retains only the least sensitive useful part
// of a validated asset URL for transient terminal output.
func downloadHostname(sourceURL string) string {
	parsed, err := url.Parse(sourceURL)
	if err != nil || parsed.Hostname() == "" {
		return "remote host"
	}
	return parsed.Hostname()
}

// formatElapsed keeps permanent download records consistent with the transient
// throbber without exposing the cli package's private rendering helper.
func formatElapsed(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	return elapsed.Truncate(time.Second).String()
}

// exclusiveWriter lets several goroutines share one terminal line. Every write
// reaching it is a complete frame or a complete record, so serializing whole
// writes is all the coordination the shared line needs.
type exclusiveWriter struct {
	mutex  sync.Mutex
	writer io.Writer
}

func (writer *exclusiveWriter) Write(data []byte) (int, error) {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.writer.Write(data)
}

// exclusiveFile keeps a terminal recognizable through the lock in front of it.
// Whether animation is possible is a question about the underlying stream, and
// a wrapper that hid its file descriptor would answer it as "not a terminal".
type exclusiveFile struct {
	*exclusiveWriter
	fd uintptr
}

func (writer *exclusiveFile) Fd() uintptr { return writer.fd }

// newExclusiveWriter guards output, exposing a file descriptor only when the
// wrapped writer actually has one.
func newExclusiveWriter(output io.Writer) io.Writer {
	guarded := &exclusiveWriter{writer: output}
	file, ok := output.(interface{ Fd() uintptr })
	if !ok {
		return guarded
	}
	return &exclusiveFile{exclusiveWriter: guarded, fd: file.Fd()}
}
