package cli

import (
	"context"
	"fmt"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/dac"
	"github.com/tomdoesdev/dac/internal/output/style"
	"github.com/tomdoesdev/kit/bytesize"
)

// cacheRemoveCommand builds the targeted removal.
// Cache removal accepts coordinates and resolves their digests for the caller.
func (runner *runner) cacheRemoveCommand() *urfave.Command {
	return &urfave.Command{
		Name:      "remove",
		Usage:     "Remove the objects specific asset versions resolved to.",
		ArgsUsage: "<namespace>/<name>@<version>...",
		Flags: []urfave.Flag{
			&urfave.BoolFlag{Name: "force", Usage: "Accept uncaching an asset that shares an object with one being removed."},
		},
		Action: runner.run("cache.remove", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			names, err := coordinates(current)
			if err != nil {
				return nil, "", err
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.CacheRemove(ctx, dac.CacheRemoveOptions{
				Coordinates: names,
				Force:       current.Bool("force"),
			})
			if err != nil {
				return nil, "", err
			}
			return result, cacheRemoveText(runner.stdoutPalette, result), nil
		}),
	}
}

// cacheRemoveText summarizes one targeted removal.
func cacheRemoveText(palette style.Palette, result dac.CacheRemoveResult) string {
	var text strings.Builder
	_, _ = fmt.Fprintf(&text, "Removed %s (%s).",
		palette.Strong(plural(result.ObjectCount, "object")), bytesize.Humanize(result.ByteCount))
	if len(result.Shared) > 0 {
		_, _ = fmt.Fprintf(&text, " %s also lost cached bytes.", palette.Name(strings.Join(result.Shared, ", ")))
	}
	if len(result.Missing) > 0 {
		_, _ = fmt.Fprintf(&text, " %s.", palette.Warn(plural(len(result.Missing), "object")+" was not cached"))
	}
	return text.String()
}
