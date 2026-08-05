// Package cli defines the DAC command-line interface.
package cli

import (
	"context"
	"fmt"
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
	// A failure that never reached a command action -- an unknown command, an
	// argument the parser refused -- has had no palette settled for it, and it
	// still has to be legible.
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
	// args is the command line this run was given, which the writer decision
	// below has to read before urfave has parsed anything.
	args []string
	// colour is the --color value as it was written. It is checked where a bad
	// argument is reported rather than where it is read: see styles.
	colour string
	// The palettes for this run's two streams: command summaries go to standard
	// output, and error messages, help, and progress go to standard error.
	stdoutPalette style.Palette
	stderrPalette style.Palette

	settings *config.Config
	loadOnce sync.Once
	loadErr  error
}

// styles settles how this run colours each of its two streams.
//
// They are settled separately because they are separate destinations, and
// routinely different kinds of destination: `dac pull > log` leaves a terminal
// on standard error and a file on standard output, so one decision for the
// process would fill the file with sequences to match the terminal.
//
// JSON mode colours neither. Standard output carries a parsed contract, and the
// human error message that would have carried the colour on standard error is
// not written at all.
//
// A value neither this nor style.ParseMode understands falls back to auto here
// rather than failing, because this runs on the path that reports the failure.
// checkColor is where a bad one is refused.
func (runner *runner) styles() {
	if runner.json {
		runner.stdoutPalette, runner.stderrPalette = style.Palette{}, style.Palette{}
		return
	}
	mode, _ := style.ParseMode(runner.colour)
	runner.stdoutPalette = style.New(runner.stdout, mode)
	runner.stderrPalette = style.New(runner.stderr, mode)
}

// checkColor refuses a --color value DAC does not understand, rather than
// leaving somebody who typed --color=alwyas to conclude their terminal is at
// fault.
func (runner *runner) checkColor() error {
	if _, err := style.ParseMode(runner.colour); err != nil {
		return fault.Wrap("invalid_arguments", "The color option is invalid.", err)
	}
	return nil
}

// completing reports whether this run is about shell completion rather than
// about an asset.
//
// Two invocations are, and they are easy to mistake for each other: the
// completion command, which writes the script a shell sources, and the hidden
// flag that script sends back to ask what the next word could be.
//
// Both have to reach standard output, and neither did. urfave sends both
// through Root().Writer, which DAC points at standard error so that help never
// lands on the stream carrying the output contract. So `dac completion bash`
// wrote its script where `$(...)` could not capture it, and the suggestions
// went where every generated script discards them -- bash asks through
// `$(... 2>/dev/null)`, zsh and fish do the same. The feature installed
// cleanly and did nothing, with nothing printed to say so.
//
// Help never prints during either one, so the two uses of Writer never collide.
func completing(app *urfave.Command, args []string) bool {
	if slices.Contains(args, "--"+urfave.GenerateShellCompletionFlag.Names()[0]) {
		return true
	}
	return commandName(app, args) == completionCommand
}

// completionCommand is the name urfave gives the command that writes a shell
// completion script. It builds that command itself, so this is the one place
// DAC has to spell it.
const completionCommand = "completion"

// commandName returns the first argument naming a command, stepping over the
// global options in front of it and the values they take.
//
// It reads the option set from the command rather than from a list kept beside
// it, so a global option added later is accounted for by existing.
func commandName(app *urfave.Command, args []string) string {
	for index := 0; index < len(args); index++ {
		value := args[index]
		if !strings.HasPrefix(value, "-") {
			return value
		}
		// An option spelled --name=value carries its own value; one spelled
		// --name takes the argument after it, unless it is a boolean.
		if !strings.Contains(value, "=") && takesValue(app.Flags, value) {
			index++
		}
	}
	return ""
}

// takesValue reports whether an option is followed by its value. Every option
// is except a boolean one, which carries its answer in whether it is present.
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
			// Colour is a global flag for the reason --json is one: it says how
			// a result is written rather than what it says, so every command
			// answers to it. The alias is there because the option is spelled
			// color everywhere a script would expect to find it -- git, ls,
			// grep, NO_COLOR itself -- and this project is written in the other
			// English.
			&urfave.StringFlag{
				Name:        "color",
				Aliases:     []string{"colour"},
				Sources:     urfave.EnvVars("DAC_COLOR"),
				Value:       "auto",
				Destination: &runner.colour,
				Usage:       "Colour human-readable output: auto, always, or never.",
			},
			// Tracing is a global flag because the question it answers -- what
			// did this actually do -- is asked of whichever command just
			// surprised somebody, and having to find out which of them carries
			// it is part of the problem.
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
		runner.packCommand(),
		runner.unpackCommand(),
		runner.cacheCommand(),
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

func (runner *runner) helpOrInvalid(_ context.Context, current *urfave.Command) error {
	name := current.Args().First()
	if name == "" {
		return urfave.ShowRootCommandHelp(current)
	}
	runner.commandName = name
	runner.usage = true
	if !runner.json {
		_ = urfave.ShowRootCommandHelp(current)
	}
	// Only at the root, which is where the retired spellings were. This is also
	// the action a bare `dac cache <nonsense>` reaches, and answering that with
	// where a top-level command went would be a non sequitur.
	if moved, exists := movedCommands[name]; exists && current == current.Root() {
		return fault.New("invalid_arguments", moved)
	}
	return fault.New("invalid_arguments", "The command is not valid.")
}

// movedCommands maps each command version 8 retired to where its work went.
//
// It is the same reasoning movedFlags is written from: telling somebody with
// `dac import` in a script that there is no such command is true and useless,
// and the one question they have is where it went. Both of these are answered
// by one archive doing the job two used to.
var movedCommands = map[string]string{
	"import": "dac import is now dac cache import, and it reads a dacpack rather than a cache bundle.",
	"export": "dac export is now dac pack. A dacpack carries the same objects, and dac cache import installs them.",
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
	"--timeout":        "transfer.timeout",
	"--retries":        "transfer.retries",
	"--download-parts": "transfer.download-parts",
	"--max-size":       "transfer.max-size",
	"--progress":       "transfer.progress, or --no-progress for one run",
	"--distdir":        "an argument to dac cache import",
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
