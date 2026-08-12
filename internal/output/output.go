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

// Options controls successful output without changing error visibility.
type Options struct {
	JSON  bool `flag:"json" help:"write structured JSON output"`
	Quiet bool `flag:"quiet,q" help:"suppress successful human output"`
}

// Validate rejects output modes whose guarantees cannot both be honored.
func (options Options) Validate() error {
	if options.JSON && options.Quiet {
		return errors.New("--json and --quiet cannot be combined")
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
	options *Options
	stdout  io.Writer
}

// New creates an output writer bound to the invocation's global options.
func New(options *Options, stdout io.Writer) *Writer {
	return &Writer{options: options, stdout: stdout}
}

// ValidateOptions lets each command participate in cli's normal validation
// lifecycle without duplicating global option rules.
func (writer *Writer) ValidateOptions() error { return writer.options.Validate() }

// Success writes a successful command result in the selected output mode.
func (writer *Writer) Success(command string, paths project.Paths, assets []Result, warnings []string) error {
	if writer.options.JSON {
		return json.NewEncoder(writer.stdout).Encode(successOutput{Version: project.Version, Command: command, Project: paths.Root, Assets: assets, Warnings: warnings})
	}
	if writer.options.Quiet {
		return nil
	}
	for _, asset := range assets {
		if _, err := fmt.Fprintf(writer.stdout, "%s %s\n", asset.Status, asset.Name); err != nil {
			return err
		}
	}
	return nil
}

// Error writes an invocation error and returns its conventional CLI exit code.
func (writer *Writer) Error(stderr io.Writer, err error) int {
	var usage *cli.UsageError
	if errors.As(err, &usage) {
		if writer.options.JSON {
			_ = json.NewEncoder(stderr).Encode(errorOutput{Version: project.Version, Kind: "usage", Message: sanitizeError(usage.Error())})
		} else {
			_, _ = fmt.Fprintf(stderr, "Error: %s\n\n%s", sanitizeError(usage.Error()), usage.Usage())
		}
		return 2
	}
	var operation *project.Error
	if !errors.As(err, &operation) {
		operation = &project.Error{Kind: "filesystem", Err: err}
	}
	if writer.options.JSON {
		_ = json.NewEncoder(stderr).Encode(errorOutput{Version: project.Version, Kind: operation.Kind, Message: sanitizeError(operation.Err.Error()), Asset: operation.Asset, Hint: operation.Hint, Expected: operation.Expected, Received: operation.Received})
	} else {
		_, _ = fmt.Fprintf(stderr, "Error: %s\n", sanitizeError(operation.Error()))
		if operation.Hint != "" {
			_, _ = fmt.Fprintln(stderr, operation.Hint)
		}
	}
	if operation.Kind == "configuration" {
		return 2
	}
	return 1
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
