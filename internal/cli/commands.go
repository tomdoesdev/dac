package cli

// This file defines the commands that take no network options, along with the
// counting helper their summaries share.

import (
	"context"
	"fmt"

	urfave "github.com/urfave/cli/v3"

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

func (runner *runner) verifyCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "verify",
		Usage: "Check that the project files agree.",
		Action: runner.run("verify", func(_ context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
				return nil, "", err
			}
			result, err := runner.projectService(current).Verify()
			return result, "Project files are valid.", err
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
			return result, fmt.Sprintf("Removed %s@%s.", name, version), err
		}),
	}
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
		Usage: "Copy locked objects into a distribution directory.",
		Flags: []urfave.Flag{
			&urfave.StringFlag{Name: "dir", Required: true, Usage: "Write objects to this directory."},
		},
		Action: runner.run("export", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
				return nil, "", err
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.Export(ctx, current.String("dir"))
			if err != nil {
				return nil, "", err
			}
			return result, fmt.Sprintf("Exported %s (%s).", plural(result.AssetCount, "asset"), bytesize.Format(result.ByteCount)), nil
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
