package cli

// This file defines the commands that take no network options, along with the
// counting helper their summaries share.

import (
	"context"
	"fmt"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/bytesize"
	"github.com/tom/dac/internal/coord"
)

func (runner *runner) initCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "init",
		Usage: "Create matching empty project files.",
		Flags: []urfave.Flag{&urfave.BoolFlag{Name: "force", Usage: "Replace existing project files."}},
		Action: runner.run("init", func(_ context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
				return nil, "", err
			}
			result, err := runner.projectService(current).Init(current.Bool("force"))
			return result, "Created project files.", err
		}),
	}
}

func (runner *runner) removeCommand() *urfave.Command {
	return &urfave.Command{
		Name:      "remove",
		Usage:     "Remove one asset version and update the lock file.",
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
			return result, removeText(name, result), nil
		}),
	}
}

// removeText summarizes one removal. It says which versions of the asset
// survived, because a removal now takes one version rather than the asset, and
// an operator who meant to retire the whole thing should find that out here.
//
// A removal makes no request, so it can leave the lock file describing less
// than the manifest does, and the summary says which assets rather than letting
// the next command be the one to mention it.
func removeText(name coord.Coordinate, result application.RemoveResult) string {
	text := fmt.Sprintf("Removed %s.", name)
	if len(result.Remaining) > 0 {
		text += fmt.Sprintf(" %s still has %s.", name.Group(), strings.Join(result.Remaining, ", "))
	}
	if len(result.Unlocked) > 0 {
		text += fmt.Sprintf(" The lock file does not describe %s. Run dac pull --update-lock.", strings.Join(result.Unlocked, ", "))
	}
	return text
}

func (runner *runner) pathCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "path",
		Usage: "Get one verified object path.",
		// The version can be left off when the project carries one, which is
		// what makes this usable inside a shell substitution without repeating
		// a version that is already in the manifest.
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

func (runner *runner) exportCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "export",
		Usage: "Write locked objects to a cache bundle.",
		Flags: []urfave.Flag{
			&urfave.StringFlag{Name: "file", Required: true, Usage: "Write the tar bundle to this file."},
		},
		Action: runner.run("export", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
				return nil, "", err
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.Export(ctx, current.String("file"))
			if err != nil {
				return nil, "", err
			}
			return result, fmt.Sprintf("Exported %s (%s).", plural(result.AssetCount, "asset"), bytesize.Format(result.ByteCount)), nil
		}),
	}
}

// importCommand builds the local cache bundle import command.
func (runner *runner) importCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "import",
		Usage: "Install objects from a cache bundle.",
		Flags: []urfave.Flag{
			&urfave.StringFlag{Name: "file", Required: true, Usage: "Read the tar bundle from this file."},
		},
		Action: runner.run("import", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
				return nil, "", err
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.Import(ctx, current.String("file"))
			if err != nil {
				return nil, "", err
			}
			return result, fmt.Sprintf("Imported %s (%s).", plural(result.ObjectCount, "object"), bytesize.Format(result.ByteCount)), nil
		}),
	}
}

// packCommand builds the materialized archive command.
//
// The archive is an optional argument rather than a required flag, which is the
// one way this differs from export at the command line. A cache bundle is a
// thing you are moving somewhere and the destination is the point of writing
// it; a dacpack is a build output a project makes one of, and a required flag
// would have every script that touches one invent a spelling for the same file.
func (runner *runner) packCommand() *urfave.Command {
	return &urfave.Command{
		Name:      "pack",
		Usage:     "Write locked assets to a dacpack under the names their origins give them.",
		ArgsUsage: "[<archive>]",
		Action: runner.run("pack", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			paths, err := optionalPaths(current, 1, "Specify at most one archive path, or none for "+application.DefaultPackFile+".")
			if err != nil {
				return nil, "", err
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.Pack(ctx, pathOr(paths, 0, application.DefaultPackFile))
			if err != nil {
				return nil, "", err
			}
			return result, fmt.Sprintf("Packed %s (%s) into %s.",
				plural(result.AssetCount, "asset"), bytesize.Format(result.ByteCount), result.Pack), nil
		}),
	}
}

// unpackCommand builds the materialization command.
//
// It writes files and never touches the cache, so it needs no cache directory
// and reads no project files -- it runs anywhere the archive does. The
// destination defaults to the working directory, which is why --force exists:
// replacing files somebody is standing in the middle of is not something to do
// because an archive said so.
func (runner *runner) unpackCommand() *urfave.Command {
	return &urfave.Command{
		Name:      "unpack",
		Usage:     "Write the assets a dacpack carries into a directory.",
		ArgsUsage: "[<archive> [<directory>]]",
		Flags: []urfave.Flag{
			&urfave.BoolFlag{Name: "force", Usage: "Replace files that are already in the destination."},
		},
		Action: runner.run("unpack", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			paths, err := optionalPaths(current, 2,
				"Specify at most an archive path and a destination directory, as [<archive> [<directory>]].")
			if err != nil {
				return nil, "", err
			}
			result, err := runner.projectService(current).Unpack(ctx, application.UnpackOptions{
				Pack:      pathOr(paths, 0, application.DefaultPackFile),
				Directory: pathOr(paths, 1, "."),
				Force:     current.Bool("force"),
			})
			if err != nil {
				return nil, "", err
			}
			return result, fmt.Sprintf("Unpacked %s (%s) into %s.",
				plural(result.FileCount, "file"), bytesize.Format(result.ByteCount), result.Directory), nil
		}),
	}
}

// plural writes a count with its noun, because "1 objects" reads as a bug in
// the tool rather than as a summary of it.
func plural(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", count, noun)
}
