package cli

import (
	"context"
	"fmt"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/dac"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/output/style"
	"github.com/tomdoesdev/kit/bytesize"
)

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
		Action: runner.runStore("cache.scrub", func(ctx context.Context, current *urfave.Command, service *dac.Service) (any, string, error) {
			result, err := service.VerifyCache(ctx, dac.VerifyCacheOptions{
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

// scrubText summarizes one explicit cache check.
// Damage counts use warning styles because they also make scrub fail.
func scrubText(palette style.Palette, result dac.VerifyCacheResult) string {
	var text strings.Builder
	_, _ = fmt.Fprintf(&text, "Checked %s (%s).",
		palette.Strong(plural(result.Checked, "object")), bytesize.Humanize(result.ByteCount))
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
		text.WriteString(" ")
		text.WriteString(palette.Good("No damage found."))
	}
	return text.String()
}
