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
		Usage:     "Remove one asset and update the lock file.",
		ArgsUsage: "<name>@<version>",
		Action: runner.run("remove", func(_ context.Context, current *urfave.Command) (any, string, error) {
			name, version, err := coordinate(current)
			if err != nil {
				return nil, "", err
			}
			result, err := runner.projectService(current).Remove(name, version)
			if err != nil {
				return nil, "", err
			}
			return result, removeText(name, version, result), nil
		}),
	}
}

// removeText summarizes one removal. A removal makes no request, so it can
// leave the lock file describing less than the manifest does, and the summary
// says which assets rather than letting the next command be the one to mention
// it.
func removeText(name, version string, result application.RemoveResult) string {
	text := fmt.Sprintf("Removed %s@%s.", name, version)
	if len(result.Unlocked) > 0 {
		text += fmt.Sprintf(" The lock file does not describe %s. Run dac pull --update-lock.", strings.Join(result.Unlocked, ", "))
	}
	return text
}

func (runner *runner) pathCommand() *urfave.Command {
	return &urfave.Command{
		Name:      "path",
		Usage:     "Get one verified object path.",
		ArgsUsage: "<name>@<version>",
		Action: runner.run("path", func(_ context.Context, current *urfave.Command) (any, string, error) {
			name, version, err := coordinate(current)
			if err != nil {
				return nil, "", err
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.Path(name, version)
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

// plural writes a count with its noun, because "1 objects" reads as a bug in
// the tool rather than as a summary of it.
func plural(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", count, noun)
}
