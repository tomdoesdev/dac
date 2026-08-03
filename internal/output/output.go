// Package output writes command results.
package output

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/tom/dac/internal/fault"
)

const Version = 1

type envelope struct {
	OutputVersion int         `json:"outputVersion"`
	OK            bool        `json:"ok"`
	Command       string      `json:"command"`
	Data          any         `json:"data,omitempty"`
	Error         *errorValue `json:"error,omitempty"`
}

type errorValue struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// Writer writes human or JSON command results.
type Writer struct {
	stdout io.Writer
	stderr io.Writer
	json   bool
}

// New creates a command writer for the selected output mode.
func New(stdout, stderr io.Writer, jsonOutput bool) *Writer {
	return &Writer{stdout: stdout, stderr: stderr, json: jsonOutput}
}

// Success writes one command result.
func (writer *Writer) Success(command string, data any, human string) error {
	if !writer.json {
		_, err := fmt.Fprintln(writer.stdout, human)
		return err
	}
	return json.NewEncoder(writer.stdout).Encode(envelope{OutputVersion: Version, OK: true, Command: command, Data: data})
}

// Failure writes one command error.
func (writer *Writer) Failure(command string, err error) error {
	value := fault.As(err)
	if !writer.json {
		_, writeErr := fmt.Fprintf(writer.stderr, "Error: %s\n", value.Message)
		return writeErr
	}
	details := value.Details
	if details == nil {
		details = map[string]any{}
	}
	return json.NewEncoder(writer.stdout).Encode(envelope{
		OutputVersion: Version,
		OK:            false,
		Command:       command,
		Error:         &errorValue{Code: value.Code, Message: value.Message, Details: details},
	})
}
