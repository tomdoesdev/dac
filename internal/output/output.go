// Package output writes command results.
package output

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/tom/dac/internal/fault"
)

// Version is the output contract version. It became 2 when info moved its
// counters under a summary object and dropped the allowed count, and when every
// error gained a cause field. It became 3 when assets grew a namespace and a
// whole coordinate, add stopped reporting a retired coordinate because adding a
// version no longer retires one, and remove started reporting the versions it
// left behind. It became 4 when the lock operations moved off pull onto their
// own command: pull dropped the locked array it could no longer populate, and
// lock arrived reporting that array along with whether the file it writes
// actually moved.
const Version = 4

type envelope struct {
	OutputVersion int         `json:"outputVersion"`
	OK            bool        `json:"ok"`
	Command       string      `json:"command"`
	Data          any         `json:"data,omitempty"`
	Error         *errorValue `json:"error,omitempty"`
}

// errorValue is one command failure.
//
// Cause carries the underlying detail: the HTTP status, the refused connection,
// the digest that actually arrived. Code and Message are stable enough to
// branch on, which is exactly why neither can say anything specific about the
// failure, and a consumer left with only those two has to reproduce the command
// by hand to learn what went wrong.
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

// Failure writes one command error, including whatever caused it.
func (writer *Writer) Failure(command string, err error) error {
	value := fault.As(err)
	if !writer.json {
		// Error() appends the cause to the message. A bare message reads well
		// and diagnoses nothing: "The asset request failed" is true of a refused
		// connection, a 404, and an expired certificate alike.
		_, writeErr := fmt.Fprintf(writer.stderr, "Error: %s\n", value.Error())
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
