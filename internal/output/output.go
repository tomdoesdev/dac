package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"

	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/kit/cli"
)

var urlToken = regexp.MustCompile(`https?://[^\s"']+`)

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
	options      *Options
	stdout       io.Writer
	stderr       io.Writer
	stdoutStyler *cli.Styler
	stderrStyler *cli.Styler
}

// New creates an output writer bound to the invocation's global options.
func New(options *Options, stdout, stderr io.Writer) *Writer {
	return &Writer{
		options:      options,
		stdout:       stdout,
		stderr:       stderr,
		stdoutStyler: cli.NewStyler(stdout, cli.ColorAuto),
		stderrStyler: cli.NewStyler(stderr, cli.ColorAuto),
	}
}

// ValidateOptions lets each command participate in cli's normal validation
// lifecycle without duplicating global option rules.
func (writer *Writer) ValidateOptions() error { return writer.options.Validate() }

// Success writes a successful command result in the selected output mode.
func (writer *Writer) Success(command string, paths project.Paths, assets []Result, warnings []string) error {
	safeWarnings := make([]string, len(warnings))
	for index, warning := range warnings {
		safeWarnings[index] = sanitizeError(warning)
	}
	if writer.options.JSON {
		return json.NewEncoder(writer.stdout).Encode(successOutput{Version: Version, Command: command, Project: paths.Root, Assets: assets, Warnings: safeWarnings})
	}
	if !writer.options.Quiet {
		for _, asset := range assets {
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

// Status writes a complete observational report without turning unhealthy
// states into command failures.
func (writer *Writer) Status(paths project.Paths, assets []Result, orphans []Orphan) error {
	if writer.options.JSON {
		return json.NewEncoder(writer.stdout).Encode(successOutput{Version: Version, Command: "status", Project: paths.Root, Assets: assets, Orphans: orphans})
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

// Error writes an invocation error and returns its conventional CLI exit code.
func (writer *Writer) Error(stderr io.Writer, err error) int {
	styler := cli.NewStyler(stderr, cli.ColorAuto)
	var usage *cli.UsageError
	if errors.As(err, &usage) {
		if writer.options.JSON {
			_ = json.NewEncoder(stderr).Encode(errorOutput{Version: Version, Kind: "usage", Message: sanitizeError(usage.Error())})
		} else {
			_, _ = fmt.Fprintf(stderr, "%s %s\n\n%s", styler.Error("Error:"), sanitizeError(usage.Error()), usage.Usage())
		}
		return 2
	}
	var operation *project.Error
	if !errors.As(err, &operation) {
		wrapped := project.NewFilesystemError(err)
		if !errors.As(wrapped, &operation) {
			return 1
		}
	}
	if writer.options.JSON {
		_ = json.NewEncoder(stderr).Encode(errorOutput{Version: Version, Kind: string(operation.Kind()), Message: sanitizeError(operation.Message()), Asset: operation.Asset(), Hint: operation.Hint(), Expected: operation.Expected(), Received: operation.Received()})
	} else {
		_, _ = fmt.Fprintf(stderr, "%s %s\n", styler.Error("Error:"), sanitizeError(operation.Error()))
		if operation.Hint() != "" {
			_, _ = fmt.Fprintln(stderr, operation.Hint())
		}
	}
	if operation.Kind() == project.ErrorKindConfiguration {
		return 2
	}
	return 1
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

// sanitizeError removes URL credentials, query strings, and fragments from
// third-party parser and transport diagnostics before they reach either mode.
func sanitizeError(message string) string {
	return urlToken.ReplaceAllStringFunc(message, func(value string) string {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" {
			return "remote URL"
		}
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		return parsed.String()
	})
}
