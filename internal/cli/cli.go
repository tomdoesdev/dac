// Package cli defines the DAC command-line interface.
package cli

import (
	"context"
	"io"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/output"
)

const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
)

// Run runs one DAC command and returns its exit status.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	runner := &runner{stdout: stdout, stderr: stderr}
	err := runner.app().Run(ctx, append([]string{"dac"}, args...))
	if err == nil {
		return ExitOK
	}
	if runner.writeFailed {
		return ExitFailure
	}
	if writeErr := output.New(stdout, stderr, runner.json).Failure(runner.commandName, err); writeErr != nil {
		return ExitFailure
	}
	if runner.usage || fault.As(err).Code == "invalid_arguments" {
		return ExitUsage
	}
	return ExitFailure
}

type runner struct {
	stdout      io.Writer
	stderr      io.Writer
	json        bool
	commandName string
	usage       bool
	writeFailed bool
}

type action func(context.Context, *urfave.Command) (any, string, error)

func (runner *runner) app() *urfave.Command {
	app := &urfave.Command{
		Name:            "dac",
		Usage:           "Lock and cache reproducible assets.",
		Description:     "DAC stores remote files by their SHA-256 digest.",
		Version:         application.Version,
		HideHelpCommand: true,
		Writer:          runner.stderr,
		ErrWriter:       runner.stderr,
		Flags: []urfave.Flag{
			&urfave.StringFlag{Name: "manifest", Value: "dac.json", Usage: "Use this manifest file."},
			&urfave.StringFlag{Name: "lock", Value: "dac-lock.json", Usage: "Use this lock file."},
			&urfave.StringFlag{Name: "cache-dir", Sources: urfave.EnvVars("DAC_CACHE_DIR"), Usage: "Use this cache directory."},
			&urfave.BoolFlag{Name: "json", Aliases: []string{"j"}, Destination: &runner.json, Usage: "Write command results as JSON."},
		},
		Action: runner.helpOrInvalid,
	}
	app.Commands = []*urfave.Command{
		runner.initCommand(),
		runner.addCommand(),
		runner.removeCommand(),
		runner.infoCommand(),
		runner.lockCommand(),
		runner.pullCommand(),
		runner.pathCommand(),
		runner.verifyCommand(),
		runner.exportCommand(),
		runner.cacheCommand(),
	}
	_ = app.Walk(func(current *urfave.Command) error {
		current.OnUsageError = runner.usageError
		return nil
	})
	return app
}

func (runner *runner) run(name string, operation action) urfave.ActionFunc {
	return func(ctx context.Context, current *urfave.Command) error {
		runner.commandName = name
		result, summary, err := operation(ctx, current)
		if err != nil {
			return err
		}
		if err := output.New(runner.stdout, runner.stderr, runner.json).Success(name, result, summary); err != nil {
			runner.writeFailed = true
			return err
		}
		return nil
	}
}

func (runner *runner) helpOrInvalid(_ context.Context, current *urfave.Command) error {
	if !current.Args().Present() {
		return urfave.ShowRootCommandHelp(current)
	}
	runner.commandName = current.Args().First()
	runner.usage = true
	if !runner.json {
		_ = urfave.ShowRootCommandHelp(current)
	}
	return fault.New("invalid_arguments", "The command is not valid.")
}

func (runner *runner) usageError(_ context.Context, current *urfave.Command, err error, _ bool) error {
	runner.commandName = strings.TrimPrefix(strings.Join(current.Path()[1:], "."), ".")
	runner.usage = true
	if !runner.json {
		if current == current.Root() {
			_ = urfave.ShowRootCommandHelp(current)
		} else {
			_ = urfave.ShowSubcommandHelp(current)
		}
	}
	return fault.Wrap("invalid_arguments", "The command arguments are invalid.", err)
}
