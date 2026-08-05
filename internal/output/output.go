// Package output writes command results.
package output

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/style"
)

// Version identifies the JSON output contract.
const Version = 8

type envelope struct {
	OutputVersion int         `json:"outputVersion"`
	OK            bool        `json:"ok"`
	Command       string      `json:"command"`
	Data          any         `json:"data,omitempty"`
	Error         *errorValue `json:"error,omitempty"`
}

// errorValue is one command failure.
// Cause carries the underlying detail: the HTTP status, the refused connection, the digest that actually arrived.
type errorValue struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Cause   string         `json:"cause,omitempty"`
	Details map[string]any `json:"details"`
}

// Writer writes human or JSON command results.
type Writer struct {
	stdout io.Writer
	stderr io.Writer
	json   bool
	// palette styles standard error, which is the only stream this writer composes text for.
	palette style.Palette
}

// New creates a command writer for the selected output mode.
func New(stdout, stderr io.Writer, jsonOutput bool, palette style.Palette) *Writer {
	return &Writer{stdout: stdout, stderr: stderr, json: jsonOutput, palette: palette}
}

// Success writes one command result.
func (writer *Writer) Success(command string, data any, human string) error {
	if !writer.json {
		_, err := fmt.Fprintln(writer.stdout, human)
		return err
	}
	return json.NewEncoder(writer.stdout).Encode(envelope{OutputVersion: Version, OK: true, Command: command, Data: data})
}

// Failure writes one command error, including whatever caused it.
func (writer *Writer) Failure(command string, err error) error {
	value := fault.As(err)
	if !writer.json {
		// Error() appends the cause to the message.
		// Only the label is coloured.
		_, writeErr := fmt.Fprintf(writer.stderr, "%s %s\n", writer.palette.Bad("Error:"), value.Error())
		return writeErr
	}
	details := value.Details
	if details == nil {
		details = map[string]any{}
	}
	cause := ""
	if value.Cause != nil {
		cause = value.Cause.Error()
	}
	return json.NewEncoder(writer.stdout).Encode(envelope{
		OutputVersion: Version,
		OK:            false,
		Command:       command,
		Error:         &errorValue{Code: value.Code, Message: value.Message, Cause: cause, Details: details},
	})
}
