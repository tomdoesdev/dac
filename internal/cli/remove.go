package cli

import (
	"context"
	"fmt"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/output/style"
)

// removeCommand defines the command that removes one manifest asset version.
func (runner *runner) removeCommand() *urfave.Command {
	return &urfave.Command{
		Name:      "remove",
		Usage:     "Remove one asset version from the manifest without changing the lock file.",
		ArgsUsage: "<namespace>/<name>@<version>",
		Action: runner.run("remove", func(_ context.Context, current *urfave.Command) (any, string, error) {
			name, err := coordinate(current)
			if err != nil {
				return nil, "", err
			}
			result, err := runner.projectService(current).Remove(name)
			if err != nil {
				return nil, "", err
			}
			return result, removeText(runner.stdoutPalette, name, result), nil
		}),
	}
}

// removeText reports the versions that remain and the pull needed to reconcile lock state.
func removeText(palette style.Palette, name coord.Coordinate, result application.RemoveResult) string {
	text := fmt.Sprintf("Removed %s.", palette.Name(name.String()))
	if len(result.Remaining) > 0 {
		text += fmt.Sprintf(" %s still has %s.",
			palette.Name(name.Group().String()), palette.Name(strings.Join(result.Remaining, ", ")))
	}
	text += " " + palette.Warn("The lock file is unchanged. Run dac pull.")
	return text
}
