package cli

// This file defines commands that do not need network options.

import (
	"context"
	"fmt"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/bytesize"
	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/style"
)

func (runner *runner) initCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "init",
		Usage: "Create matching empty project files.",
		Flags: []urfave.Flag{&urfave.BoolFlag{Name: "force", Usage: "Replace existing project files."}},
		Action: runner.runBare("init", func(_ context.Context, current *urfave.Command) (any, string, error) {
			result, err := runner.projectService(current).Init(current.Bool("force"))
			return result, "Created project files.", err
		}),
	}
}

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

func (runner *runner) pathCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "path",
		Usage: "Get one verified object path.",
		// A group is valid when the project has only one matching version.
		ArgsUsage: "<namespace>/<name>[@<version>]",
		Action: runner.run("path", func(_ context.Context, current *urfave.Command) (any, string, error) {
			choice, err := asset(current)
			if err != nil {
				return nil, "", err
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.Path(choice)
			return result, result.Path, err
		}),
	}
}

// unpackCommand builds the cache materialization command.
func (runner *runner) unpackCommand() *urfave.Command {
	return &urfave.Command{
		Name:      "unpack",
		Usage:     "Write cached project assets into a directory, or the assets named.",
		ArgsUsage: "[<namespace>/<name>[@<version>]...]",
		Flags: []urfave.Flag{
			&urfave.StringFlag{Name: "dest", Value: ".", Usage: "Write the files into this directory."},
			&urfave.BoolFlag{Name: "force", Usage: "Replace files that are already in the destination."},
		},
		Action: runner.run("unpack", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			assets, err := selections(current)
			if err != nil {
				return nil, "", err
			}
			directory, err := destination(current)
			if err != nil {
				return nil, "", err
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.Unpack(ctx, application.UnpackOptions{
				Directory: directory,
				Assets:    assets,
				Force:     current.Bool("force"),
			})
			if err != nil {
				return nil, "", err
			}
			return result, unpackText(runner.stdoutPalette, result), nil
		}),
	}
}

// unpackText reports when a selection wrote part of the project.
func unpackText(palette style.Palette, result application.UnpackResult) string {
	size := bytesize.Format(result.ByteCount)
	if result.FileCount < result.ProjectCount {
		return fmt.Sprintf("Unpacked %s of %d (%s) into %s.",
			palette.Strong(plural(result.FileCount, "file")), result.ProjectCount, size, result.Directory)
	}
	return fmt.Sprintf("Unpacked %s (%s) into %s.",
		palette.Strong(plural(result.FileCount, "file")), size, result.Directory)
}

// plural writes a count with its singular or plural noun.
func plural(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", count, noun)
}
