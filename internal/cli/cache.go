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
	"github.com/tom/dac/internal/fault"
)

func (runner *runner) cacheCommand() *urfave.Command {
	return &urfave.Command{
		Name:            "cache",
		Usage:           "Inspect and collect the object cache.",
		HideHelpCommand: true,
		Commands: []*urfave.Command{{
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
		}},
		Action: runner.helpOrInvalid,
	}
}

// gcText summarizes one cache collection.
func gcText(result application.GCResult) string {
	verb := "Removed"
	if result.DryRun {
		verb = "Would remove"
	}
	objects := plural(result.ObjectCount, "object")
	if result.TempCount > 0 {
		return fmt.Sprintf("%s %s (%s) and %s.", verb, objects,
			bytesize.Format(result.ByteCount), plural(result.TempCount, "temporary file"))
	}
	return fmt.Sprintf("%s %s (%s).", verb, objects, bytesize.Format(result.ByteCount))
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
