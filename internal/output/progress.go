package output

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"

	"github.com/mattn/go-isatty"
	"github.com/mattn/go-runewidth"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

// Download identifies one artifact for progress presentation. It carries only
// what a terminal line may show, so presentation never reaches into transfer
// or lock state to describe a download.
type Download struct {
	File string
	URL  string
}

// DownloadProgress reports bytes staged for one active transfer. A negative
// total means the server did not declare the complete response size.
type DownloadProgress func(completed, total int64)

// DownloadGroup presents downloads that may run at the same time. mpb owns the
// synchronization needed to render one bar per active transfer safely.
type DownloadGroup struct {
	writer   *Writer
	progress *mpb.Progress
	layout   downloadLayout
}

// downloadLayout fixes the batch-derived columns that precede each bar. mpb
// assigns the remaining terminal width to every filler, so equal decorator
// widths also keep the bars and trailing metrics aligned.
type downloadLayout struct {
	fileWidth int
	hostWidth int
}

const (
	columnGap              = 2
	downloadGlyphWidth     = 1 + columnGap
	downloadSizeWidth      = 10
	downloadSeparatorWidth = 1 + columnGap
	downloadTotalWidth     = downloadSizeWidth + columnGap
	downloadPercentWidth   = 4 + columnGap
	downloadRateWidth      = 10 + columnGap
)

// WithDownloads runs operation while presenting each active download. The
// progress container shares the operation context so cancellation stops both
// network work and rendering; Wait and Shutdown also join every renderer before
// the command returns.
func (writer *Writer) WithDownloads(ctx context.Context, pending []Download, operation func(context.Context, *DownloadGroup) error) error {
	group := &DownloadGroup{writer: writer, layout: measureDownloadLayout(pending)}
	if len(pending) == 0 || !writer.rendersProgress() {
		return operation(ctx, group)
	}

	options := []mpb.ContainerOption{
		mpb.WithOutput(writer.stderr),
		mpb.WithRefreshRate(writer.progressRefresh),
		mpb.WithQueueLen(len(pending)),
		// Completed downloads become permanent records above active bars rather
		// than continuing to participate in every subsequent refresh.
		mpb.PopCompletedMode(),
	}
	if writer.progressMode == progressAlways {
		options = append(options, mpb.WithAutoRefresh())
	}
	group.progress = mpb.NewWithContext(ctx, options...)

	finished := false
	defer func() {
		// A panic must not strand mpb's renderer goroutines. Shutdown joins
		// them, then the original panic continues normally.
		if !finished {
			group.progress.Shutdown()
		}
	}()

	operationErr := operation(ctx, group)
	if operationErr != nil {
		group.progress.Shutdown()
		finished = true
		return errors.Join(operationErr, group.progress.Error)
	}
	group.progress.Wait()
	finished = true
	return group.progress.Error
}

// measureDownloadLayout accounts for display cells rather than bytes because
// opaque filenames may contain wide Unicode characters. The extra cells are
// the inter-column gap, included in the fixed width so every row starts its
// next column at exactly the same position.
func measureDownloadLayout(pending []Download) downloadLayout {
	var layout downloadLayout
	for _, item := range pending {
		layout.fileWidth = max(layout.fileWidth, runewidth.StringWidth(item.File))
		layout.hostWidth = max(layout.hostWidth, runewidth.StringWidth(downloadHostname(item.URL)))
	}
	layout.fileWidth += columnGap
	layout.hostWidth += columnGap
	return layout
}

// rendersProgress applies output-mode and terminal policy before creating mpb.
// Avoiding the container entirely for redirected output also avoids changing
// the existing stdout summary contract for scripts and pipelines.
func (writer *Writer) rendersProgress() bool {
	if writer.options.JSON || writer.options.Quiet || writer.progressMode == progressNever {
		return false
	}
	if writer.progressMode == progressAlways {
		return true
	}
	file, ok := writer.stderr.(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	fd := file.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}

// addDownload creates a byte-progress bar when a worker starts. It begins with
// an unknown total because the response has not arrived yet; the downloader's
// first progress event makes it determinate when the host declares a size.
func (group *DownloadGroup) addDownload(item Download) (*mpb.Bar, error) {
	styler := group.writer.stderrStyler
	hostname := downloadHostname(item.URL)
	barStyle := mpb.BarStyle().
		Lbound("[").
		Filler("=").
		Tip(">").
		Padding("-").
		Rbound("]").
		FillerMeta(styler.Progress).
		TipMeta(styler.Progress).
		Build()
	glyph := decor.OnAbort(
		decor.OnCompleteMeta(
			decor.OnComplete(
				decor.Meta(decor.Name("↓", decor.WC{W: downloadGlyphWidth, C: decor.DindentRight}), styler.Progress),
				"✔",
			),
			styler.Success,
		),
		"",
	)
	file := decor.OnAbort(decor.Name(item.File, decor.WC{W: group.layout.fileWidth, C: decor.DindentRight}), "")
	host := decor.OnAbort(decor.Name(hostname, decor.WC{W: group.layout.hostWidth, C: decor.DindentRight}), "")

	return group.progress.Add(
		0,
		barStyle,
		// mpb marks aborted bars as terminal too, so both filler and status
		// explicitly disappear rather than looking like a completed download.
		mpb.BarFillerClearOnAbort(),
		mpb.PrependDecorators(glyph, file, host),
		mpb.AppendDecorators(
			decor.OnAbort(decor.Any(func(statistics decor.Statistics) string {
				return fmt.Sprintf("%s", decor.SizeB1024(statistics.Current))
			}, decor.WC{W: downloadSizeWidth}), ""),
			decor.OnAbort(decor.Name("/", decor.WC{W: downloadSeparatorWidth, C: decor.DindentRight}), ""),
			decor.OnAbort(decor.Any(func(statistics decor.Statistics) string {
				if statistics.Total <= 0 {
					return "—"
				}
				return fmt.Sprintf("%s", decor.SizeB1024(statistics.Total))
			}, decor.WC{W: downloadTotalWidth, C: decor.DindentRight}), ""),
			decor.OnAbort(decor.Any(func(statistics decor.Statistics) string {
				if statistics.Total <= 0 {
					return "--%"
				}
				percentage := int64(math.Round(float64(statistics.Current) * 100 / float64(statistics.Total)))
				return fmt.Sprintf("%d%%", min(percentage, 100))
			}, decor.WC{W: downloadPercentWidth}), ""),
			decor.OnAbort(decor.AverageSpeed(decor.SizeB1024(0), "%.1f", decor.WC{W: downloadRateWidth}), ""),
		),
	)
}

// Run performs one download and completes its bar only on success. Failed bars
// are dropped so the command's classified error remains the durable record.
// mpb permits concurrent Add and bar updates, so Run is safe for worker pools.
func (group *DownloadGroup) Run(ctx context.Context, item Download, operation func(context.Context, DownloadProgress) error) (bool, error) {
	if group.progress == nil {
		return false, operation(ctx, nil)
	}
	bar, err := group.addDownload(item)
	if err != nil {
		// The renderer can observe a signal before the worker reaches the
		// downloader. Keep that race classified as cancellation instead of
		// exposing mpb.ErrDone as an unrelated rendering failure.
		if cause := context.Cause(ctx); cause != nil {
			return false, fault.NewCancelledError(cause)
		}
		return false, err
	}
	progress := func(completed, total int64) {
		if total >= 0 {
			bar.SetTotal(total, false)
		}
		bar.SetCurrent(completed)
	}
	if err := operation(ctx, progress); err != nil {
		bar.Abort(true)
		return false, err
	}
	bar.SetTotal(-1, true)
	return true, nil
}

// WithDownloadProgress presents one download on its own. A single asset is the
// batch of one, so an invocation that downloads nothing concurrently still
// shares the presentation with the one that does.
func (writer *Writer) WithDownloadProgress(ctx context.Context, filename, sourceURL string, operation func(context.Context, DownloadProgress) error) (bool, error) {
	item := Download{File: filename, URL: sourceURL}
	var reported bool
	err := writer.WithDownloads(ctx, []Download{item}, func(ctx context.Context, group *DownloadGroup) error {
		var runErr error
		reported, runErr = group.Run(ctx, item, operation)
		return runErr
	})
	return reported, err
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
