package project

import "fmt"

// Error has a stable category that the process boundary maps to JSON and exit
// codes without having to parse human-oriented error text.
type Error struct {
	Kind     string
	Asset    string
	Hint     string
	Expected string
	Received string
	Err      error
}

func (err *Error) Error() string {
	if err.Asset != "" {
		return fmt.Sprintf("%s for asset %q: %v", err.Kind, err.Asset, err.Err)
	}
	return fmt.Sprintf("%s: %v", err.Kind, err.Err)
}

func (err *Error) Unwrap() error { return err.Err }

// NewError attaches an operation category to an error without losing its cause.
func NewError(kind string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Kind: kind, Err: err}
}
