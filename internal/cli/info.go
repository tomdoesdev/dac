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
		Usage:         "Show project asset and cache information.",
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
			result, err := service.Info(application.InfoOptions{Selection: filter})
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

// statusText adds color without changing the state value in the output.
// It leaves unknown values plain so that new states do not look successful.
func statusText(palette style.Palette, status string) string {
	switch status {
	case application.CacheCached, application.LockCurrent:
		return palette.Good(status)
	case application.CacheMissing, application.LockStale, application.CacheUnavailable:
		return palette.Warn(status)
	case application.CacheCorrupt:
		return palette.Bad(status)
	}
	return status
}
