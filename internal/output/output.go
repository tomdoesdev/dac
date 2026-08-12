package output

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
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
	Reason string `json:"reason,omitempty"`
	// Reported records that progress output already announced this asset, so
	// the summary does not repeat it. WithDownloadProgress reports whether it
	// printed; carrying the answer here keeps Success a function of its
	// arguments rather than of what the writer happens to remember.
	Reported bool `json:"-"`
}

// Orphan describes lock metadata or a download entry outside desired state.
type Orphan struct {
	Kind   string `json:"kind"`
	Name   string `json:"name,omitempty"`
	File   string `json:"file"`
	Reason string `json:"reason"`
}

type successOutput struct {
	Version  int      `json:"version"`
	Command  string   `json:"command"`
	Project  string   `json:"project"`
	Assets   []Result `json:"assets,omitempty"`
	Orphans  []Orphan `json:"orphans,omitempty"`
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
	options          *Options
	stdout           io.Writer
	stderr           io.Writer
	stdoutStyler     *cli.Styler
	stderrStyler     *cli.Styler
	throbberMode     cli.ThrobberMode
	throbberInterval time.Duration
}

// defaultThrobberInterval paces the progress animation fast enough to look
// live without redrawing more often than a terminal can usefully show.
const defaultThrobberInterval = 100 * time.Millisecond

// Option adjusts a Writer at construction.
type Option func(*Writer)

// WithThrobber overrides the progress animation, which tests pin so their
// output does not depend on wall-clock timing or on an attached terminal.
func WithThrobber(mode cli.ThrobberMode, interval time.Duration) Option {
	return func(writer *Writer) {
		writer.throbberMode = mode
		writer.throbberInterval = interval
	}
}

// New creates an output writer bound to the invocation's global options.
func New(options *Options, stdout, stderr io.Writer, opts ...Option) *Writer {
	writer := &Writer{
		options:          options,
		stdout:           stdout,
		stderr:           stderr,
		stdoutStyler:     cli.NewStyler(stdout, cli.ColorAuto),
		stderrStyler:     cli.NewStyler(stderr, cli.ColorAuto),
		throbberMode:     cli.ThrobberAuto,
		throbberInterval: defaultThrobberInterval,
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

// WithDownloadProgress runs operation with DAC's human download presentation.
// Non-interactive, structured, and quiet invocations fall back to Success
// output. It reports whether it announced the completed download, which the
// caller records on the Result so the summary does not repeat it.
func (writer *Writer) WithDownloadProgress(ctx context.Context, filename, sourceURL string, operation func(context.Context) error) (bool, error) {
	mode := writer.throbberMode
	if writer.options.JSON || writer.options.Quiet {
		mode = cli.ThrobberNever
	}
	source := downloadHostname(sourceURL)
	message := fmt.Sprintf("Downloading %s from %s…", filename, source)
	var reported bool
	throbber := cli.NewThrobber(
		writer.stderr,
		message,
		cli.WithThrobberMode(mode),
		cli.WithThrobberInterval(writer.throbberInterval),
		cli.WithThrobberStyle(writer.stderrStyler.Progress),
		cli.WithThrobberOnEnd(func(end cli.ThrobberEnd) string {
			if !end.Animated || end.Err != nil || writer.options.JSON || writer.options.Quiet {
				return ""
			}
			reported = true
			return fmt.Sprintf("%s %s %s from %s (%s)",
				writer.stderrStyler.Success("✔"),
				filename,
				writer.stderrStyler.Success("downloaded"),
				source,
				formatElapsed(end.Elapsed))
		}),
	)
	err := throbber.Run(ctx, operation)
	return reported, err
}

// downloadHostname deliberately retains only the least sensitive useful part
// of a validated asset URL for transient terminal output.
func downloadHostname(sourceURL string) string {
	parsed, err := url.Parse(sourceURL)
	if err != nil || parsed.Hostname() == "" {
		return "remote host"
	}
	return parsed.Hostname()
}

// formatElapsed keeps permanent download records consistent with the transient
// throbber without exposing the cli package's private rendering helper.
func formatElapsed(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	return elapsed.Truncate(time.Second).String()
}

// Status writes a complete observational report without turning unhealthy
// states into command failures.
func (writer *Writer) Status(root string, assets []Result, orphans []Orphan) error {
	if writer.options.JSON {
		return json.NewEncoder(writer.stdout).Encode(successOutput{Version: Version, Command: "status", Project: root, Assets: assets, Orphans: orphans})
	}
	if writer.options.Quiet {
		return nil
	}
	for _, item := range assets {
		if _, err := fmt.Fprintf(writer.stdout, "%s %s", writer.status(item.Status), item.Name); err != nil {
			return err
		}
		if item.Reason != "" {
			if _, err := fmt.Fprintf(writer.stdout, ": %s", item.Reason); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(writer.stdout); err != nil {
			return err
		}
	}
	for _, orphan := range orphans {
		label := orphan.Name
		if orphan.Kind == "file" {
			label = fmt.Sprintf("file %q", orphan.File)
		}
		if _, err := fmt.Fprintf(writer.stdout, "%s %s: %s\n", writer.stdoutStyler.Warning("orphaned"), label, orphan.Reason); err != nil {
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

// status maps DAC's observational states onto cli's semantic roles without
// changing the stable status strings used by JSON and command logic.
func (writer *Writer) status(status string) string {
	switch status {
	case "verified":
		return writer.stdoutStyler.Success(status)
	case "invalid":
		return writer.stdoutStyler.Error(status)
	case "missing", "stale":
		return writer.stdoutStyler.Warning(status)
	default:
		return status
	}
}
