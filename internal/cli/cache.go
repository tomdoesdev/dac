package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	urfave "github.com/urfave/cli/v3"

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/bytesize"
	"github.com/tom/dac/internal/cache"
	"github.com/tom/dac/internal/fault"
)

func (runner *runner) cacheCommand() *urfave.Command {
	return &urfave.Command{
		Name:            "cache",
		Usage:           "Inspect, check, and collect the object cache.",
		HideHelpCommand: true,
		Commands: []*urfave.Command{
			runner.cacheGCCommand(),
			runner.cacheVerifyCommand(),
			runner.cacheDirCommand(),
		},
		Action: runner.helpOrInvalid,
	}
}

func (runner *runner) cacheGCCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "gc",
		Usage: "Remove cache objects that nothing has used recently.",
		Flags: []urfave.Flag{
			&urfave.StringFlag{Name: "max-age", Value: "30d", Sources: urfave.EnvVars("DAC_MAX_AGE"), Usage: "Keep objects used within this period."},
			&urfave.BoolFlag{Name: "dry-run", Usage: "Report what collection would remove."},
		},
		Action: runner.run("cache.gc", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
				return nil, "", err
			}
			maxAge, err := parseAge(current.String("max-age"))
			if err != nil {
				return nil, "", fault.Wrap("invalid_arguments", "The maximum age is invalid.", err)
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.CacheGC(ctx, application.GCOptions{MaxAge: maxAge, DryRun: current.Bool("dry-run")})
			if err != nil {
				return nil, "", err
			}
			return result, gcText(result), nil
		}),
	}
}

func (runner *runner) cacheVerifyCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "verify",
		Usage: "Hash cache objects and report the ones that no longer match their digest.",
		Flags: []urfave.Flag{
			&urfave.BoolFlag{Name: "all", Usage: "Check every object in the cache instead of this project's."},
			&urfave.BoolFlag{Name: "repair", Usage: "Remove the objects that fail."},
		},
		Action: runner.run("cache.verify", func(ctx context.Context, current *urfave.Command) (any, string, error) {
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
			if result.CorruptCount > 0 && !current.Bool("repair") {
				return nil, "", &fault.Error{
					Code:    "cache_object_corrupt",
					Message: verifyText(result) + " Run dac cache verify --repair, then dac pull.",
					Details: map[string]any{"corrupt": result.Corrupt},
				}
			}
			return result, verifyText(result), nil
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
			root, err := cache.ResolveRoot(current.String("cache-dir"))
			if err != nil {
				return nil, "", fault.Wrap("cache_root_unresolved", "DAC could not resolve the cache directory.", err)
			}
			return application.CacheDirResult{Path: root}, root, nil
		}),
	}
}

// verifyText summarizes one explicit cache check.
func verifyText(result application.VerifyCacheResult) string {
	var text strings.Builder
	_, _ = fmt.Fprintf(&text, "Checked %s (%s).", plural(result.Checked, "object"), bytesize.Format(result.ByteCount))
	if result.MissingCount > 0 {
		_, _ = fmt.Fprintf(&text, " %s missing.", plural(result.MissingCount, "object"))
	}
	if result.CorruptCount > 0 {
		_, _ = fmt.Fprintf(&text, " %s corrupt.", plural(result.CorruptCount, "object"))
	}
	if result.Repaired > 0 {
		_, _ = fmt.Fprintf(&text, " Removed %s.", plural(result.Repaired, "corrupt object"))
	}
	if result.CorruptCount == 0 && result.MissingCount == 0 {
		text.WriteString(" No damage found.")
	}
	return text.String()
}

// gcText summarizes one cache collection.
func gcText(result application.GCResult) string {
	verb := "Removed"
	if result.DryRun {
		verb = "Would remove"
	}
	var text strings.Builder
	_, _ = fmt.Fprintf(&text, "%s %s (%s)", verb, plural(result.ObjectCount, "object"), bytesize.Format(result.ByteCount))
	if result.TempCount > 0 {
		_, _ = fmt.Fprintf(&text, ", %s", plural(result.TempCount, "temporary file"))
	}
	if result.SidecarCount > 0 {
		_, _ = fmt.Fprintf(&text, ", %s", plural(result.SidecarCount, "orphaned sidecar"))
	}
	text.WriteByte('.')
	return text.String()
}

// ageUnits extends Go durations with the periods a cache lifetime is actually
// written in. Nobody sets a cache policy in hours.
var ageUnits = map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour}

func parseAge(value string) (time.Duration, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, errors.New("the maximum age is empty")
	}
	if unit, exists := ageUnits[trimmed[len(trimmed)-1:]]; exists {
		count, err := strconv.ParseFloat(trimmed[:len(trimmed)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("age %q is invalid", value)
		}
		if count < 0 {
			return 0, fmt.Errorf("age %q must not be negative", value)
		}
		return time.Duration(count * float64(unit)), nil
	}
	age, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("age %q is invalid", value)
	}
	if age < 0 {
		return 0, fmt.Errorf("age %q must not be negative", value)
	}
	return age, nil
}
