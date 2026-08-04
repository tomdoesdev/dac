// Package cli defines the DAC command-line interface.
package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	urfave "github.com/urfave/cli/v3"

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/config"
	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/output"
)

const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
)

// The project file names, which are also the defaults their flags carry.
const (
	DefaultManifest = "dac.json"
	DefaultLock     = "dac-lock.json"
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

	settings *config.Config
	loadOnce sync.Once
	loadErr  error
}

// config reads the configuration for this run, once.
//
// It is lazy because the path it reads comes from a flag, so it cannot be
// settled before parsing, and because the commands that touch neither the
// network nor the cache have no reason to read a file at all -- dac unpack runs
// anywhere the archive does, and a config it never consults should not be able
// to stop it.
func (runner *runner) config(current *urfave.Command) (*config.Config, error) {
	runner.loadOnce.Do(func() {
		runner.settings, runner.loadErr = config.Load(current.String("config"))
		if runner.loadErr != nil {
			runner.loadErr = fault.Wrap("config_invalid", "The configuration is invalid.", runner.loadErr)
		}
	})
	return runner.settings, runner.loadErr
}

type action func(context.Context, *urfave.Command) (any, string, error)

func (runner *runner) app() *urfave.Command {
	app := &urfave.Command{
		Name:            "dac",
		Usage:           "Lock and cache reproducible assets.",
		Description:     "DAC stores remote files by their SHA-256 digest.",
		Version:         application.Version,
		HideHelpCommand: true,
		// Completion writes to stdout by design, so it is the one command whose
		// output is not a DAC command result. It stays outside the output
		// contract for that reason: a shell evaluates it, nothing parses it.
		EnableShellCompletion: true,
		Writer:                runner.stderr,
		ErrWriter:             runner.stderr,
		Flags: []urfave.Flag{
			&urfave.StringFlag{Name: "manifest", Value: DefaultManifest, Usage: "Use this manifest file."},
			&urfave.StringFlag{Name: "lock", Value: DefaultLock, Usage: "Use this lock file. Defaults beside the manifest."},
			&urfave.StringFlag{Name: "cache-dir", Sources: urfave.EnvVars("DAC_CACHE_DIR"), Usage: "Use this cache directory."},
			&urfave.StringFlag{Name: "config", Sources: urfave.EnvVars("DAC_CONFIG"), Usage: "Read this config file instead of the ones the XDG search path finds."},
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
		runner.importCommand(),
		runner.packCommand(),
		runner.unpackCommand(),
		runner.cacheCommand(),
		runner.configCommand(),
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
	if moved := movedFlag(err); moved != "" {
		return fault.Wrap("invalid_arguments",
			fmt.Sprintf("The %s option moved into the config file, as %s. Run dac config path to find it.", moved, movedFlags[moved]), err)
	}
	return fault.Wrap("invalid_arguments", "The command arguments are invalid.", err)
}

// movedFlags maps each option that version 7 took off the command line to the
// config key that replaced it.
//
// Answering "that is not a flag" for an option somebody has in a script would
// be true and useless. These say where the setting went, which is the one
// question an operator hitting this actually has.
var movedFlags = map[string]string{
	"--timeout":           "transfer.timeout",
	"--retries":           "transfer.retries",
	"--download-parts":    "transfer.download-parts",
	"--max-size":          "transfer.max-size",
	"--credential-helper": "the credentials table",
	"--progress":          "transfer.progress, or --no-progress for one run",
	"--distdir":           "an argument to dac import",
}

// movedFlag reports which retired option a parse error is about, if any.
func movedFlag(err error) string {
	text := err.Error()
	for name := range movedFlags {
		// urfave spells the failure as `flag provided but not defined: -timeout`,
		// with one dash whatever the caller wrote, so match on the bare name and
		// require a dash before it and a boundary after -- otherwise --max-size
		// would answer for --max-age.
		bare := strings.TrimLeft(name, "-")
		index := strings.Index(text, bare)
		if index <= 0 || text[index-1] != '-' {
			continue
		}
		if rest := text[index+len(bare):]; rest == "" || rest[0] == ' ' || rest[0] == '=' {
			return name
		}
	}
	return ""
}
