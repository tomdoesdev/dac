package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/dac"
	"github.com/tomdoesdev/dac/internal/output/style"
	"github.com/tomdoesdev/kit/bytesize"
)

func (runner *runner) cacheListCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "list",
		Usage: "List the objects in the cache, newest use first.",
		Flags: []urfave.Flag{
			&urfave.BoolFlag{Name: "all", Usage: "List every object in the cache instead of this project's."},
		},
		Action: runner.runStore("cache.list", func(ctx context.Context, current *urfave.Command, service *dac.Service) (any, string, error) {
			result, err := service.CacheList(ctx, dac.CacheListOptions{All: current.Bool("all")})
			if err != nil {
				return nil, "", err
			}
			return result, listText(runner.stdoutPalette, result), nil
		}),
	}
}

// listText summarizes one cache listing.
// Cache listing puts coordinates after object metadata so each row is easy to scan.
func listText(palette style.Palette, result dac.CacheListResult) string {
	if result.ObjectCount == 0 {
		if result.MissingCount > 0 {
			return fmt.Sprintf("No objects. %s. %s",
				palette.Warn(plural(result.MissingCount, "asset")+" not cached"),
				palette.Warn("Run dac pull."))
		}
		return "No objects."
	}
	var text strings.Builder
	for _, object := range result.Objects {
		_, _ = fmt.Fprintf(&text, "%s  %s  %s",
			palette.Detail(object.Digest),
			palette.Detail(fmt.Sprintf("%10s", bytesize.Humanize(object.Size))),
			palette.Detail(object.LastUsed.Format(time.RFC3339)))
		if object.Filename != "" {
			_, _ = fmt.Fprintf(&text, "  %s", palette.Detail(object.Filename))
		}
		// What this object is now is stated in the project's own colour. What it once was is
		// stated in the same muted colour as the rest of the record, because a coordinate no
		// project names any more is history rather than an answer.
		if len(object.Coordinates) > 0 {
			_, _ = fmt.Fprintf(&text, "  %s", palette.Name(strings.Join(object.Coordinates, ", ")))
		} else if len(object.KnownAs) > 0 {
			_, _ = fmt.Fprintf(&text, "  %s", palette.Detail(strings.Join(object.KnownAs, ", ")))
		}
		text.WriteByte('\n')
	}
	_, _ = fmt.Fprintf(&text, "%s (%s)",
		palette.Strong(plural(result.ObjectCount, "object")), bytesize.Humanize(result.ByteCount))
	text.WriteByte('.')
	if result.MissingCount > 0 {
		_, _ = fmt.Fprintf(&text, " %s.", palette.Warn(plural(result.MissingCount, "asset")+" not cached"))
	}
	// The count of objects nothing can explain is the number this listing exists to drive to zero.
	if result.UnknownCount > 0 {
		_, _ = fmt.Fprintf(&text, " %s.", palette.Warn(plural(result.UnknownCount, "object")+" unrecognized"))
	}
	return text.String()
}
