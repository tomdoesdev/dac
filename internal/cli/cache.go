package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/bytesize"
	"github.com/tomdoesdev/dac/internal/config"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/style"
)

func (runner *runner) cacheCommand() *urfave.Command {
	return &urfave.Command{
		Name:            "cache",
		Usage:           "Inspect, check, and collect the object cache.",
		HideHelpCommand: true,
		Commands: []*urfave.Command{
			runner.cacheDirCommand(),
			runner.cacheListCommand(),
			runner.cacheGCCommand(),
			runner.cacheClearCommand(),
			runner.cacheRemoveCommand(),
			runner.cacheScrubCommand(),
		},
		Action: runner.helpOrInvalid,
	}
}

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
		Action: runner.run("cache.gc", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
				return nil, "", err
			}
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
			result, err := service.CacheGC(ctx, application.GCOptions{
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

// cacheScrubCommand builds the object integrity check.
// Scrub names the full cache hash check and differs from project verification.
func (runner *runner) cacheScrubCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "scrub",
		Usage: "Hash cache objects and report the ones that no longer match their digest.",
		Flags: []urfave.Flag{
			&urfave.BoolFlag{Name: "all", Usage: "Check every object in the cache instead of this project's."},
			&urfave.BoolFlag{Name: "repair", Usage: "Remove the objects that fail."},
		},
		Action: runner.run("cache.scrub", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
				return nil, "", err
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.VerifyCache(ctx, application.VerifyCacheOptions{
				All:    current.Bool("all"),
				Repair: current.Bool("repair"),
			})
			if err != nil {
				return nil, "", err
			}
			// A cache that fails its own check is a command failure.
			// The message goes into a fault, which is a value the JSON contract carries and every wrapper appends a cause to, so it is built with no palette at all.
			if result.CorruptCount > 0 && !current.Bool("repair") {
				return nil, "", &fault.Error{
					Code:    "cache_object_corrupt",
					Message: scrubText(style.Palette{}, result) + " Run dac cache scrub --repair, then dac pull.",
					Details: map[string]any{"corrupt": result.Corrupt},
				}
			}
			return result, scrubText(runner.stdoutPalette, result), nil
		}),
	}
}

func (runner *runner) cacheDirCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "dir",
		Usage: "Print the resolved cache directory.",
		Action: runner.run("cache.dir", func(_ context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
				return nil, "", err
			}
			root, err := runner.cacheRoot(current)
			if err != nil {
				return nil, "", err
			}
			return application.CacheDirResult{Path: root}, root, nil
		}),
	}
}

func (runner *runner) cacheListCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "list",
		Usage: "List the objects in the cache, newest use first.",
		Flags: []urfave.Flag{
			&urfave.BoolFlag{Name: "all", Usage: "List every object in the cache instead of this project's."},
		},
		Action: runner.run("cache.list", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
				return nil, "", err
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.CacheList(ctx, application.CacheListOptions{All: current.Bool("all")})
			if err != nil {
				return nil, "", err
			}
			return result, listText(runner.stdoutPalette, result), nil
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
		Action: runner.run("cache.clear", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
				return nil, "", err
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			// Clear needs no prompt because dry-run is available and pull restores objects.
			result, err := service.CacheClear(ctx, current.Bool("dry-run"))
			if err != nil {
				return nil, "", err
			}
			return result, gcText(runner.stdoutPalette, result), nil
		}),
	}
}

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
		ShellComplete: runner.completeCoordinates(),
		Action: runner.run("cache.remove", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			names, err := coordinates(current)
			if err != nil {
				return nil, "", err
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.CacheRemove(ctx, application.CacheRemoveOptions{
				Coordinates: names,
				Force:       current.Bool("force"),
			})
			if err != nil {
				return nil, "", err
			}
			return result, removeObjectsText(runner.stdoutPalette, result), nil
		}),
	}
}

// listText summarizes one cache listing.
// Cache listing puts coordinates after object metadata so each row is easy to scan.
func listText(palette style.Palette, result application.CacheListResult) string {
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
			palette.Detail(fmt.Sprintf("%10s", bytesize.Format(object.Size))),
			palette.Detail(object.LastUsed.Format(time.RFC3339)))
		if len(object.Coordinates) > 0 {
			_, _ = fmt.Fprintf(&text, "  %s", palette.Name(strings.Join(object.Coordinates, ", ")))
		}
		text.WriteByte('\n')
	}
	_, _ = fmt.Fprintf(&text, "%s (%s)",
		palette.Strong(plural(result.ObjectCount, "object")), bytesize.Format(result.ByteCount))
	if result.MissingCount > 0 {
		_, _ = fmt.Fprintf(&text, ". %s.", palette.Warn(plural(result.MissingCount, "asset")+" not cached"))
	} else {
		text.WriteByte('.')
	}
	return text.String()
}

// removeObjectsText summarizes one targeted removal.
func removeObjectsText(palette style.Palette, result application.CacheRemoveResult) string {
	var text strings.Builder
	_, _ = fmt.Fprintf(&text, "Removed %s (%s).",
		palette.Strong(plural(result.ObjectCount, "object")), bytesize.Format(result.ByteCount))
	if len(result.Shared) > 0 {
		_, _ = fmt.Fprintf(&text, " %s also lost cached bytes.", palette.Name(strings.Join(result.Shared, ", ")))
	}
	if len(result.Missing) > 0 {
		_, _ = fmt.Fprintf(&text, " %s.", palette.Warn(plural(len(result.Missing), "object")+" was not cached"))
	}
	return text.String()
}

// scrubText summarizes one explicit cache check.
// Damage counts use warning styles because they also make scrub fail.
func scrubText(palette style.Palette, result application.VerifyCacheResult) string {
	var text strings.Builder
	_, _ = fmt.Fprintf(&text, "Checked %s (%s).",
		palette.Strong(plural(result.Checked, "object")), bytesize.Format(result.ByteCount))
	if result.MissingCount > 0 {
		_, _ = fmt.Fprintf(&text, " %s.", palette.Warn(plural(result.MissingCount, "object")+" missing"))
	}
	if result.CorruptCount > 0 {
		_, _ = fmt.Fprintf(&text, " %s.", palette.Bad(plural(result.CorruptCount, "object")+" corrupt"))
	}
	if result.Repaired > 0 {
		_, _ = fmt.Fprintf(&text, " Removed %s.", plural(result.Repaired, "corrupt object"))
	}
	if result.CorruptCount == 0 && result.MissingCount == 0 {
		text.WriteString(" " + palette.Good("No damage found."))
	}
	return text.String()
}

// gcText summarizes one cache collection.
func gcText(palette style.Palette, result application.GCResult) string {
	verb := "Removed"
	if result.DryRun {
		verb = "Would remove"
	}
	var text strings.Builder
	_, _ = fmt.Fprintf(&text, "%s %s (%s)", verb,
		palette.Strong(plural(result.ObjectCount, "object")), bytesize.Format(result.ByteCount))
	text.WriteByte('.')
	return text.String()
}

// maximumCacheSize reads the collection size bound, preferring the flag over the config.
func (runner *runner) maximumCacheSize(current *urfave.Command) (int64, error) {
	if current.IsSet("max-size") {
		size, err := config.ParseSize(current.String("max-size"))
		if err != nil {
			return 0, fault.Wrap("invalid_arguments", "The maximum size is invalid.", err)
		}
		return size, nil
	}
	settings, err := runner.config(current)
	if err != nil {
		return 0, err
	}
	return settings.CacheMaxSize, nil
}

// maximumAge reads the collection age, preferring the flag over the config.
func (runner *runner) maximumAge(current *urfave.Command) (time.Duration, error) {
	if current.IsSet("max-age") {
		age, err := config.ParseDuration(current.String("max-age"))
		if err != nil {
			return 0, fault.Wrap("invalid_arguments", "The maximum age is invalid.", err)
		}
		return age, nil
	}
	settings, err := runner.config(current)
	if err != nil {
		return 0, err
	}
	return settings.MaxAge, nil
}
