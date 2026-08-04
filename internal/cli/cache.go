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
		// The cache is a noun with a complete set of verbs: where it is, what
		// is in it, fill it from a delivery, collect it by age, empty it, drop
		// one asset, check it. Emptying it used to be spelled as a collection
		// with an age short enough that everything fell outside it, seeing into
		// it at all was not possible, and filling it was a top-level command
		// that read a format nothing else in DAC used.
		Commands: []*urfave.Command{
			runner.cacheDirCommand(),
			runner.cacheListCommand(),
			runner.cacheImportCommand(),
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
			// The age is the parameter of the operation rather than a tuning
			// knob, so it stays a flag: a collection without one is not a
			// meaningful command. The config file supplies the default a site
			// runs on, and the flag overrides it for one run.
			&urfave.StringFlag{
				Name:        "max-age",
				Usage:       "Keep objects used within this period.",
				DefaultText: "cache.max-age, or " + config.DefaultMaxAge,
			},
			// The size bound is the other parameter of the same operation, and
			// it is a flag for the same reason the age is: what a collection
			// aims at belongs to the collection rather than to the machine,
			// even though the machine supplies the usual answer.
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
//
// It is called scrub rather than verify because dac verify already means
// something, and the two cost wildly different amounts: dac verify reads two
// JSON files, dac verify --refresh downloads every asset again, and this reads
// every byte in the cache. One word covering all three told an operator nothing
// about which one they were about to run. Scrub is what storage systems call
// reading everything to check it, and it carries the cost in the name.
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
			// A cache that fails its own check is a command failure. An operator
			// who scripted this expects a nonzero status to mean "act", and
			// finding the damage is not the same as it not being there.
			//
			// The message goes into a fault, which is a value the JSON contract
			// carries and every wrapper appends a cause to, so it is built with
			// no palette at all. Colour belongs to what a stream is written to,
			// and this is not written yet.
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

// cacheImportCommand builds the local cache import command.
//
// It is a cache verb because the cache is what it writes: it reads no project
// files, resolves nothing, and the archive it takes was made somewhere else. As
// a top-level command it sat beside pull and lock looking like part of a
// project's workflow, and the pair it actually belonged to -- export and import
// -- described a format nothing else in DAC read.
//
// What it reads now is a dacpack, the archive dac pack writes, so the machine
// that receives one can install it into a cache with this or materialize it
// with dac unpack. That is the thing a second format cost: a bundle delivered
// to somebody who did not run DAC was a tar full of files named by hash.
//
// It accepts a directory as well, for a delivery that arrives on a mounted
// share rather than as a file. Each file in it must be named by its digest,
// which is the only name a consumer can check.
func (runner *runner) cacheImportCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "import",
		Usage: "Install objects from a dacpack or a directory of digest-named files.",
		// The source is an argument rather than a required flag, and it has no
		// default: an import reads a file somebody delivered, at whatever path
		// they put it down. That is a reason to require the argument, not a
		// reason to make every script spell --file in front of it.
		ArgsUsage: "<dacpack|directory>",
		Action: runner.run("cache.import", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			source, err := requiredPath(current, "Specify one dacpack or directory path to read, as <dacpack|directory>.")
			if err != nil {
				return nil, "", err
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.Import(ctx, source)
			if err != nil {
				return nil, "", err
			}
			return result, fmt.Sprintf("Imported %s (%s).",
				runner.stdoutPalette.Strong(plural(result.ObjectCount, "object")), bytesize.Format(result.ByteCount)), nil
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
			// No confirmation prompt: DAC has none anywhere, a cleared cache
			// costs a pull rather than anything that cannot be got back, and
			// --dry-run is already the careful path.
			result, err := service.CacheClear(ctx, current.Bool("dry-run"))
			if err != nil {
				return nil, "", err
			}
			return result, gcText(runner.stdoutPalette, result), nil
		}),
	}
}

// cacheRemoveCommand builds the targeted removal.
//
// It takes coordinates rather than digests because a coordinate is what a
// person has: the cache is keyed by digest, and looking one up to forget it is
// the sort of errand a tool should run for you.
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
//
// A listing is a table of things nobody reads across: what a person wants from
// a row is which asset it is, and the digest, size, and last use in front of
// that are how the cache identifies it rather than how they do. So the columns
// recede and the coordinates at the end of each row do not.
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
//
// The counts that mean damage are the reason anybody ran this, and a scrub that
// found some is also a command failure -- so they are coloured as what they are
// rather than as more of the sentence they sit in.
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
	if result.TempCount > 0 {
		_, _ = fmt.Fprintf(&text, ", %s", plural(result.TempCount, "temporary file"))
	}
	if result.SidecarCount > 0 {
		_, _ = fmt.Fprintf(&text, ", %s", plural(result.SidecarCount, "orphaned sidecar"))
	}
	text.WriteByte('.')
	// Eviction is said separately because it means something else. Collecting
	// what nothing has used is a cache working; taking what a project still
	// wants is a cache too small for this machine, and the next command pays to
	// download it again. The leading verb already set the tense, so neither of
	// these has to say it twice.
	switch result.EvictedCount {
	case 0:
	case result.ObjectCount:
		text.WriteString(" " + palette.Warn("All of them still in use, to stay within the size bound."))
	default:
		_, _ = fmt.Fprintf(&text, " %s", palette.Warn(fmt.Sprintf(
			"%d of them (%s) still in use, to stay within the size bound.",
			result.EvictedCount, bytesize.Format(result.EvictedBytes))))
	}
	return text.String()
}

// maximumCacheSize reads the collection size bound, preferring the flag over
// the config. Zero is no bound, which is what a cache collected only by age is.
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
