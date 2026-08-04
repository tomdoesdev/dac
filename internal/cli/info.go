package cli

import (
	"context"
	"fmt"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/bytesize"
	"github.com/tomdoesdev/dac/internal/style"
)

// infoCommand defines the project inspection command and its coordinate filter.
func (runner *runner) infoCommand() *urfave.Command {
	return &urfave.Command{
		Name:          "info",
		Usage:         "Show project asset and request information.",
		ArgsUsage:     "[<namespace>/<name>[@<version>]]",
		ShellComplete: runner.completeCoordinate(),
		Action: runner.run("info", func(_ context.Context, current *urfave.Command) (any, string, error) {
			filter, err := selection(current)
			if err != nil {
				return nil, "", err
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			settings, err := runner.config(current)
			if err != nil {
				return nil, "", err
			}
			// Rewriting is never disabled here. Info reports the source URL and
			// the request URL side by side, so the un-rewritten view is already
			// on screen; a flag to suppress half of it would subtract
			// information rather than add a choice.
			rewriter, err := loadRewriteConfig(settings, service.ManifestPath, false)
			if err != nil {
				return nil, "", err
			}
			result, err := service.Info(application.InfoOptions{Selection: filter, Rewriter: rewriter})
			return result, infoText(runner.stdoutPalette, result), err
		}),
	}
}

// infoText formats each asset as one detailed information block.
//
// The keys recede and the values do not. A block is eleven lines of which two
// are usually the reason it was run, and a column of labels down the left is
// how somebody finds the line they want without reading the ones they do not.
func infoText(palette style.Palette, result application.InfoResult) string {
	if len(result.Assets) == 0 {
		return "No assets."
	}
	var text strings.Builder
	for index, asset := range result.Assets {
		if index > 0 {
			text.WriteByte('\n')
		}
		_, _ = fmt.Fprintf(&text, "%s\n", palette.Strong(asset.Coordinate))
		field := func(label, value string) {
			_, _ = fmt.Fprintf(&text, "%s %s\n", palette.Detail(label+":"), value)
		}
		field("source", asset.SourceURL)
		field("request", asset.RequestURL)
		field("policy", statusText(palette, asset.RequestStatus))
		field("lock", statusText(palette, result.Summary.LockStatus))
		field("cache", statusText(palette, asset.CacheStatus))
		if asset.Filename != "" {
			field("filename", asset.Filename)
		}
		if asset.Integrity != "" {
			field("integrity", palette.Detail(asset.Integrity))
		}
		if asset.Digest != "" {
			field("digest", palette.Detail(asset.Digest))
		}
		if asset.Size != nil {
			field("size", bytesize.Format(*asset.Size))
		}
		if asset.Path != "" {
			field("path", asset.Path)
		}
	}
	return strings.TrimSuffix(text.String(), "\n")
}

// statusText colours one of the state words info reports.
//
// The words are the ones the JSON contract carries, and this changes none of
// them -- it says which of three things each one means. A cache that holds the
// object and a lock file that describes the manifest need nothing from anybody;
// a missing object costs a pull and a stale lock costs a lock; a corrupt object
// and a blocked host are somebody's afternoon.
//
// An unrecognized word is left alone rather than guessed at, so a state added
// later reads plainly here instead of reading as good news. A lock and a cache
// both spell an absent thing "missing", so one case covers both -- naming the
// other constant beside it would be the same value twice.
func statusText(palette style.Palette, status string) string {
	switch status {
	case application.CacheCached, application.LockCurrent, application.RequestAllowed:
		return palette.Good(status)
	case application.CacheMissing, application.LockStale, application.CacheUnavailable:
		return palette.Warn(status)
	case application.CacheCorrupt, application.RequestBlocked:
		return palette.Bad(status)
	}
	return status
}
