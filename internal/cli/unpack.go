package cli

import (
	"context"
	"fmt"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/output/style"
	"github.com/tomdoesdev/kit/bytesize"
)

// unpackCommand defines the command that materializes cached project assets.
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
	size := bytesize.Humanize(result.ByteCount)
	if result.FileCount < result.ProjectCount {
		return fmt.Sprintf("Unpacked %s of %d (%s) into %s.",
			palette.Strong(plural(result.FileCount, "file")), result.ProjectCount, size, result.Directory)
	}
	return fmt.Sprintf("Unpacked %s (%s) into %s.",
		palette.Strong(plural(result.FileCount, "file")), size, result.Directory)
}
