// Package cli defines the DAC command-line interface.
package cli

import (
	"context"
	"io"
	"slices"
	"strings"
	"sync"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/config"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/output"
	"github.com/tomdoesdev/dac/internal/style"
	"github.com/tomdoesdev/dac/internal/trust"
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
	runner := &runner{stdout: stdout, stderr: stderr, args: args}
	err := runner.app().Run(ctx, append([]string{"dac"}, args...))
	if err == nil {
		return ExitOK
	}
	if runner.writeFailed {
		return ExitFailure
	}
	// Parser failures need a palette because no command action initialized one.
	runner.styles()
	if writeErr := output.New(stdout, stderr, runner.json, runner.stderrPalette).Failure(runner.commandName, err); writeErr != nil {
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
	// args is the command line this run was given, which the writer decision below has to read before urfave has parsed anything.
	args []string
	// colour is the --color value as it was written.
	colour string
	// The palettes for this run's two streams: command summaries go to standard output, and error messages, help, and progress go to standard error.
	stdoutPalette style.Palette
	stderrPalette style.Palette

	settings *config.Config
	loadOnce sync.Once
	loadErr  error

	// The trusted-hosts file this run reads, and the gate a network command built from it.
	trustStore *trust.Store
	trustList  trust.List
	trustOnce  sync.Once
	trustErr   error
	gate       *trust.Gate
}

// styles settles how this run colours each of its two streams.
// Each stream gets a palette because stdout and stderr can have different destinations.
// JSON mode colours neither.
// A value neither this nor style.ParseMode understands falls back to auto here rather than failing, because this runs on the path that reports the failure.
func (runner *runner) styles() {
	if runner.json {
		runner.stdoutPalette, runner.stderrPalette = style.Palette{}, style.Palette{}
		return
	}
	mode, _ := style.ParseMode(runner.colour)
	runner.stdoutPalette = style.New(runner.stdout, mode)
	runner.stderrPalette = style.New(runner.stderr, mode)
}

// checkColor refuses a --color value DAC does not understand, rather than leaving somebody who typed --color=alwyas to conclude their terminal is at fault.
func (runner *runner) checkColor() error {
	if _, err := style.ParseMode(runner.colour); err != nil {
		return fault.Wrap("invalid_arguments", "The color option is invalid.", err)
	}
	return nil
}

// completing reports whether this run is about shell completion rather than about an asset.
// Completion includes both the script command and the hidden suggestion request.
// Both have to reach standard output, and neither did.
// Help never prints during either one, so the two uses of Writer never collide.
func completing(app *urfave.Command, args []string) bool {
	if slices.Contains(args, "--"+urfave.GenerateShellCompletionFlag.Names()[0]) {
		return true
	}
	return commandName(app, args) == completionCommand
}

// completionCommand is the name urfave gives the command that writes a shell completion script.
const completionCommand = "completion"

// commandName returns the first argument naming a command, stepping over the global options in front of it and the values they take.
// It reads the option set from the command rather than from a list kept beside it, so a global option added later is accounted for by existing.
func commandName(app *urfave.Command, args []string) string {
	for index := 0; index < len(args); index++ {
		value := args[index]
		if !strings.HasPrefix(value, "-") {
			return value
		}
		// An option spelled --name=value carries its own value; one spelled --name takes the argument after it, unless it is a boolean.
		if !strings.Contains(value, "=") && takesValue(app.Flags, value) {
			index++
		}
	}
	return ""
}

// takesValue reports whether an option is followed by its value.
func takesValue(flags []urfave.Flag, option string) bool {
	name := strings.TrimLeft(option, "-")
	for _, flag := range flags {
		if !slices.Contains(flag.Names(), name) {
			continue
		}
		_, boolean := flag.(*urfave.BoolFlag)
		return !boolean
	}
	return false
}

// config reads the configuration for this run, once.
// It is lazy because the path comes from a parsed flag and some commands do not need configuration.
func (runner *runner) config(current *urfave.Command) (*config.Config, error) {
	runner.loadOnce.Do(func() {
		runner.settings, runner.loadErr = config.Load(current.String("config"))
		if runner.loadErr != nil {
			runner.loadErr = fault.Wrap("config_invalid", "The configuration is invalid.", runner.loadErr)
		}
	})
	return runner.settings, runner.loadErr
}

// trustFile reads the trusted-hosts file for this run, once.
// The path comes from the flag, then the config file, then the XDG data location, which is how the cache root resolves.
func (runner *runner) trustFile(current *urfave.Command) (*trust.Store, trust.List, error) {
	runner.trustOnce.Do(func() {
		selected := current.String("trust-file")
		if selected == "" {
			settings, err := runner.config(current)
			if err != nil {
				runner.trustErr = err
				return
			}
			selected = settings.TrustFile
		}
		path, err := trust.ResolvePath(selected)
		if err != nil {
			runner.trustErr = fault.Wrap("trust_file_unresolved", "DAC could not resolve the trusted-hosts file.", err)
			return
		}
		runner.trustStore = trust.New(path)
		// A file nobody has written yet trusts nothing, which is a state to report rather than a failure to read.
		runner.trustList, err = runner.trustStore.Load()
		if err != nil {
			runner.trustErr = fault.Wrap("trust_file_invalid", "The trusted-hosts file is invalid.", err)
		}
	})
	return runner.trustStore, runner.trustList, runner.trustErr
}

// flushTrust records the hosts this run downloaded from.
// A failure to write is traced rather than reported, because a use time is not the answer the command was asked for, and losing one only brings a collection forward.
func (runner *runner) flushTrust(ctx context.Context, current *urfave.Command) {
	if runner.gate == nil {
		return
	}
	// A cancelled run still reached the hosts it reached.
	if err := runner.gate.Flush(context.WithoutCancel(ctx)); err != nil {
		runner.trace(current).Debug("trusted hosts not recorded", "error", err)
	}
}

type action func(context.Context, *urfave.Command) (any, string, error)

func (runner *runner) app() *urfave.Command {
	app := &urfave.Command{
		Name:            "dac",
		Usage:           "Lock and cache reproducible assets.",
		Description:     "DAC stores remote files by their SHA-256 digest.",
		Version:         application.Version,
		HideHelpCommand: true,
		// Completion writes shell data to stdout outside the JSON contract.
		EnableShellCompletion: true,
		Writer:                runner.stderr,
		ErrWriter:             runner.stderr,
		Flags: []urfave.Flag{
			&urfave.StringFlag{Name: "manifest", Value: DefaultManifest, Usage: "Use this manifest file."},
			&urfave.StringFlag{Name: "lock", Value: DefaultLock, Usage: "Use this lock file. Defaults beside the manifest."},
			&urfave.StringFlag{Name: "cache-dir", Sources: urfave.EnvVars("DAC_CACHE_DIR"), Usage: "Use this cache directory."},
			&urfave.StringFlag{Name: "trust-file", Sources: urfave.EnvVars("DAC_TRUST_FILE"), Usage: "Use this trusted-hosts file."},
			&urfave.StringFlag{Name: "config", Sources: urfave.EnvVars("DAC_CONFIG"), Usage: "Read this config file instead of the ones the XDG search path finds."},
			&urfave.BoolFlag{Name: "json", Aliases: []string{"j"}, Destination: &runner.json, Usage: "Write command results as JSON."},
			// Colour is a global flag for the reason --json is one: it says how a result is written rather than what it says, so every command answers to it.
			&urfave.StringFlag{
				Name:        "color",
				Aliases:     []string{"colour"},
				Sources:     urfave.EnvVars("DAC_COLOR"),
				Value:       "auto",
				Destination: &runner.colour,
				Usage:       "Colour human-readable output: auto, always, or never.",
			},
			// Debug is global because every command can need request and cache traces.
			&urfave.BoolFlag{
				Name:    "debug",
				Sources: urfave.EnvVars("DAC_DEBUG"),
				Usage:   "Write a trace of requests and cache decisions to standard error.",
			},
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
		runner.unpackCommand(),
		runner.cacheCommand(),
		runner.trustCommand(),
		runner.configCommand(),
	}
	// The writer is settled once the flags exist to read it from. See completing.
	if completing(app, runner.args) {
		app.Writer = runner.stdout
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
		runner.styles()
		if err := runner.checkColor(); err != nil {
			return err
		}
		result, summary, err := operation(ctx, current)
		// Every command completes here, which is the one place a run has finished
		// using the hosts it was going to use, whether or not it succeeded.
		runner.flushTrust(ctx, current)
		if err != nil {
			return err
		}
		if err := output.New(runner.stdout, runner.stderr, runner.json, runner.stderrPalette).Success(name, result, summary); err != nil {
			runner.writeFailed = true
			return err
		}
		return nil
	}
}

// helpOrInvalid answers an invocation that names no operation to run: dac on its
// own, one of the commands that only group others, or either of those followed by
// a word that is not one of their commands.
//
// A bare command is somebody asking what it can do, so it gets help and succeeds.
// JSON mode is the exception rather than a second spelling of the same thing:
// there is no help to write into a document, and a caller that asked for one has
// to get one, so the same invocation is reported as the usage error it is.
func (runner *runner) helpOrInvalid(_ context.Context, current *urfave.Command) error {
	if name := current.Args().First(); name == "" && !runner.json {
		runner.showHelp(current)
		return nil
	}
	runner.commandName = commandPath(current)
	runner.usage = true
	if !runner.json {
		runner.showHelp(current)
	}
	return fault.New("invalid_arguments", "The command is not valid.")
}

func (runner *runner) usageError(_ context.Context, current *urfave.Command, err error, _ bool) error {
	runner.commandName = commandPath(current)
	runner.usage = true
	if !runner.json {
		runner.showHelp(current)
	}
	return fault.Wrap("invalid_arguments", "The command arguments are invalid.", err)
}

// showHelp writes the help for one command, which is the root's only when that is the command.
// A group that showed the root's help would answer "what can dac trust do" with the list of things dac can do, and never name its own commands at all.
func (runner *runner) showHelp(current *urfave.Command) {
	if current == current.Root() {
		_ = urfave.ShowRootCommandHelp(current)
		return
	}
	_ = urfave.ShowSubcommandHelp(current)
}

// commandPath names the command a usage failure happened in, in the dotted form every other result reports.
func commandPath(current *urfave.Command) string {
	return strings.TrimPrefix(strings.Join(current.Path()[1:], "."), ".")
}
