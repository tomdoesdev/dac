// Package fault defines stable command errors.
package fault

import (
	"errors"
	"fmt"
	"strings"
)

// Error contains a stable code and a user message.
type Error struct {
	Code    string
	Message string
	Details map[string]any
	Cause   error
}

func (value *Error) Error() string {
	if value.Cause == nil {
		return value.Message
	}
	// Messages are sentences and causes are clauses.
	return fmt.Sprintf("%s: %v", strings.TrimSuffix(value.Message, "."), value.Cause)
}

func (value *Error) Unwrap() error { return value.Cause }

// New creates an error with a stable code.
// Details stays nil.
func New(code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Wrap creates an error with a stable code and a cause.
func Wrap(code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

// As returns a stable error. It uses internal_error for an unknown error.
func As(err error) *Error {
	if value, ok := errors.AsType[*Error](err); ok {
		return value
	}
	return Wrap("internal_error", "DAC could not complete the command.", err)
}
