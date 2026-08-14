package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/redact"
	"github.com/tomdoesdev/kit/cli"
)

// Version is the current structured output protocol.
const Version = 1

// Options controls successful output without changing error visibility.
type Options struct {
	JSON  bool `flag:"json" help:"write structured JSON output"`
	Quiet bool `flag:"quiet,q" help:"suppress successful human output"`
}

// Validate rejects output modes whose guarantees cannot both be honored.
func (options Options) Validate() error {
	if options.JSON && options.Quiet {
		return ErrConflictingModes
	}
	return nil
}

// Result describes one asset changed or verified by a successful command.
type Result struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	File   string `json:"file,omitempty"`
	Digest string `json:"digest,omitempty"`
	Size   int64  `json:"size,omitempty"`
	// Reported records that progress output already announced this asset, so
	// the summary does not repeat it. WithDownloadProgress reports whether it
	// printed; carrying the answer here keeps Success a function of its
	// arguments rather than of what the writer happens to remember.
	Reported bool `json:"-"`
}

type successOutput struct {
	Version  int      `json:"version"`
	Command  string   `json:"command"`
	Project  string   `json:"project"`
	Assets   []Result `json:"assets,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

type errorOutput struct {
	Version  int    `json:"version"`
	Kind     string `json:"kind"`
	Message  string `json:"message"`
	Asset    string `json:"asset,omitempty"`
	Hint     string `json:"hint,omitempty"`
	Expected string `json:"expected,omitempty"`
	Received string `json:"received,omitempty"`
}

// Writer applies one invocation's output mode to command results.
type Writer struct {
	options         *Options
	stdout          io.Writer
	stderr          io.Writer
	stdoutStyler    *cli.Styler
	stderrStyler    *cli.Styler
	progressMode    progressMode
	progressRefresh time.Duration
}

// defaultProgressRefresh paces progress rendering fast enough to look
// live without redrawing more often than a terminal can usefully show.
const defaultProgressRefresh = 100 * time.Millisecond

// progressMode controls terminal detection without leaking presentation test
// hooks into commands. Production uses automatic detection; tests can force a
// renderer on or off for deterministic coverage.
type progressMode uint8

const (
	progressAuto progressMode = iota
	progressAlways
	progressNever
)

// Option adjusts a Writer at construction.
type Option func(*Writer)

// withProgress overrides progress detection and refresh timing for tests whose
// output must not depend on an attached terminal.
func withProgress(mode progressMode, interval time.Duration) Option {
	return func(writer *Writer) {
		writer.progressMode = mode
		writer.progressRefresh = interval
	}
}

// New creates an output writer bound to the invocation's global options.
func New(options *Options, stdout, stderr io.Writer, opts ...Option) *Writer {
	writer := &Writer{
		options:         options,
		stdout:          stdout,
		stderr:          stderr,
		stdoutStyler:    cli.NewStyler(stdout, cli.ColorAuto),
		stderrStyler:    cli.NewStyler(stderr, cli.ColorAuto),
		progressMode:    progressAuto,
		progressRefresh: defaultProgressRefresh,
	}
	for _, option := range opts {
		option(writer)
	}
	return writer
}

// ValidateOptions lets each command participate in cli's normal validation
// lifecycle without duplicating global option rules.
func (writer *Writer) ValidateOptions() error { return writer.options.Validate() }

// Success writes a successful command result in the selected output mode.
func (writer *Writer) Success(command, root string, assets []Result, warnings []string) error {
	safeWarnings := make([]string, len(warnings))
	for index, warning := range warnings {
		safeWarnings[index] = redact.URLs(warning)
	}
	if writer.options.JSON {
		return json.NewEncoder(writer.stdout).Encode(successOutput{Version: Version, Command: command, Project: root, Assets: assets, Warnings: safeWarnings})
	}
	if !writer.options.Quiet {
		for _, asset := range assets {
			if asset.Reported {
				continue
			}
			if _, err := fmt.Fprintf(writer.stdout, "%s %s\n", writer.stdoutStyler.Success(asset.Status), asset.Name); err != nil {
				return err
			}
		}
	}
	for _, warning := range safeWarnings {
		if _, err := fmt.Fprintf(writer.stderr, "%s %s\n", writer.stderrStyler.Warning("Warning:"), warning); err != nil {
			return err
		}
	}
	return nil
}

// Error writes an invocation error in the selected output mode. Choosing the
// process exit status from it is the caller's decision, not the writer's.
func (writer *Writer) Error(err error) {
	var usage *cli.UsageError
	if errors.As(err, &usage) {
		if writer.options.JSON {
			_ = json.NewEncoder(writer.stderr).Encode(errorOutput{Version: Version, Kind: "usage", Message: redact.URLs(usage.Error())})
		} else {
			_, _ = fmt.Fprintf(writer.stderr, "%s %s\n\n%s", writer.stderrStyler.Error("Error:"), redact.URLs(usage.Error()), usage.Usage())
		}
		return
	}
	var operation *fault.Error
	if !errors.As(err, &operation) {
		// An unclassified failure still reached the process boundary, so report
		// it under the category that assumes the least about its cause.
		if !errors.As(fault.NewFilesystemError(err), &operation) {
			return
		}
	}
	recovery := formatRecovery(operation.Recovery())
	if writer.options.JSON {
		_ = json.NewEncoder(writer.stderr).Encode(errorOutput{Version: Version, Kind: string(operation.Kind()), Message: redact.URLs(operation.Message()), Asset: operation.Asset(), Hint: recovery, Expected: operation.Expected(), Received: operation.Received()})
		return
	}
	_, _ = fmt.Fprintf(writer.stderr, "%s %s\n", writer.stderrStyler.Error("Error:"), redact.URLs(operation.Error()))
	if recovery != "" {
		_, _ = fmt.Fprintln(writer.stderr, recovery)
	}
}

// formatRecovery renders a structured recovery as one command a user can paste
// into a shell. Asset names are opaque and may contain shell metacharacters or
// begin with a dash, so they are quoted and follow the option terminator.
func formatRecovery(recovery fault.Recovery) string {
	if recovery.Empty() {
		return ""
	}
	words := make([]string, 0, len(recovery.Flags)+len(recovery.Assets)+3)
	words = append(words, "dac", recovery.Command)
	words = append(words, recovery.Flags...)
	if len(recovery.Assets) > 0 {
		words = append(words, "--")
		for _, name := range recovery.Assets {
			words = append(words, shellQuote(name))
		}
	}
	return "run: " + strings.Join(words, " ")
}

// shellQuote returns one POSIX shell word that reproduces value exactly.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
