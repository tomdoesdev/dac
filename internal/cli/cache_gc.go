package cli

import (
	"context"
	"fmt"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/config"
	"github.com/tomdoesdev/dac/internal/dac"
	"github.com/tomdoesdev/dac/internal/output/style"
	"github.com/tomdoesdev/kit/bytesize"
)

func (runner *runner) cacheGCCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "gc",
		Usage: "Remove cache objects that nothing has used recently.",
		Flags: []urfave.Flag{
			// The age is the parameter of the operation rather than a tuning knob, so it stays a flag: a collection without one is not a meaningful command.
			&urfave.StringFlag{
				Name:        "max-age",
				Usage:       "Keep objects used within this period.",
				DefaultText: "cache.max-age, or " + config.DefaultMaxAge,
			},
			// The command flag overrides the configured size goal for one collection.
			&urfave.StringFlag{
				Name:        "max-size",
				Usage:       "Evict the least recently used objects until the cache is this size, or none.",
				DefaultText: "cache.max-size, or " + config.DefaultCacheMaxSize,
			},
			&urfave.BoolFlag{Name: "dry-run", Usage: "Report what collection would remove."},
		},
		Action: runner.runBare("cache.gc", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			maxAge, err := runner.maximumAge(current)
			if err != nil {
				return nil, "", err
			}
			maxSize, err := runner.maximumCacheSize(current)
			if err != nil {
				return nil, "", err
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.CacheGC(ctx, dac.GCOptions{
				MaxAge:  maxAge,
				MaxSize: maxSize,
				DryRun:  current.Bool("dry-run"),
			})
			if err != nil {
				return nil, "", err
			}
			return result, gcText(runner.stdoutPalette, result), nil
		}),
	}
}

func (runner *runner) cacheClearCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "clear",
		Usage: "Remove every object in the cache.",
		Flags: []urfave.Flag{
			&urfave.BoolFlag{Name: "dry-run", Usage: "Report what clearing would remove."},
		},
		Action: runner.runStore("cache.clear", func(ctx context.Context, current *urfave.Command, service *dac.Service) (any, string, error) {
			// Clear needs no prompt because dry-run is available and pull restores objects.
			result, err := service.CacheClear(ctx, current.Bool("dry-run"))
			if err != nil {
				return nil, "", err
			}
			return result, gcText(runner.stdoutPalette, result), nil
		}),
	}
}

// gcText summarizes one cache collection.
func gcText(palette style.Palette, result dac.GCResult) string {
	verb := "Removed"
	if result.DryRun {
		verb = "Would remove"
	}
	var text strings.Builder
	_, _ = fmt.Fprintf(&text, "%s %s (%s)", verb,
		palette.Strong(plural(result.ObjectCount, "object")), bytesize.Humanize(result.ByteCount))
	text.WriteByte('.')
	return text.String()
}
