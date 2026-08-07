package cli

import (
	"context"

	urfave "github.com/urfave/cli/v3"
)

// initCommand defines the command that creates the project's initial files.
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
