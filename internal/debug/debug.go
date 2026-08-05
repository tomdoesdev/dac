// Package debug carries the optional trace logger DAC writes when tracing is on.
//
// A trace reports request, retry, range, and cache decisions. It helps an
// operator find the cause of a transfer failure.
//
// Nothing here is part of the output contract. Trace lines go to standard error,
// they are for a person reading them, and their wording is free to change.
package debug

import (
	"io"
	"log/slog"
)

// Discard is the logger a component uses when nothing turned tracing on. It is
// a real logger rather than a nil one, so no call site has to guard.
var Discard = slog.New(slog.DiscardHandler)

// New returns the logger for one run: a trace to writer, or Discard.
func New(writer io.Writer, enabled bool) *slog.Logger {
	if !enabled {
		return Discard
	}
	return slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// Or returns logger, or Discard when there is none. It is what a component
// built without one calls, so that the zero value of every struct holding a
// logger is a component that traces nothing rather than one that panics.
func Or(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return Discard
	}
	return logger
}
