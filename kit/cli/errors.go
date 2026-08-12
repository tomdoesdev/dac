package cli

import (
	"errors"
	"fmt"
)

var (
	// ErrAlreadyRun marks an application invoked more than once.
	ErrAlreadyRun = errors.New("cli: app has already run")
	// ErrInvalidCommandDefinition marks an invalid command, group, flag, or pattern registration.
	ErrInvalidCommandDefinition = errors.New("cli: invalid command definition")
	// ErrInvalidInvocation marks command-line input or validation rejected before execution.
	ErrInvalidInvocation = errors.New("cli: invalid invocation")
	// ErrCompletionDisabled marks completion requested without enabling the feature.
	ErrCompletionDisabled = errors.New("cli: completion is not enabled")
	// ErrUnsupportedShell marks a completion target the CLI cannot generate.
	ErrUnsupportedShell = errors.New("cli: unsupported completion shell")
	// ErrOutput marks a failure while writing CLI-owned diagnostics.
	ErrOutput = errors.New("cli: output failed")
)

// classifiedError preserves established human-readable diagnostics while
// exposing a stable category and, when present, the original cause.
type classifiedError struct {
	kind    error
	cause   error
	message string
}

func (err *classifiedError) Error() string { return err.message }

func (err *classifiedError) Unwrap() []error {
	if err.cause == nil {
		return []error{err.kind}
	}
	return []error{err.kind, err.cause}
}

// newCLIError formats contextual detail without making callers depend on its
// wording for classification.
func newCLIError(kind error, format string, arguments ...any) error {
	return &classifiedError{kind: kind, message: fmt.Sprintf(format, arguments...)}
}

// wrapCLIError adds stable classification while retaining an underlying error
// for errors.Is and errors.As checks.
func wrapCLIError(kind, cause error, format string, arguments ...any) error {
	return &classifiedError{kind: kind, cause: cause, message: fmt.Sprintf(format, arguments...)}
}
