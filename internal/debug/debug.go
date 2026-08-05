// Package debug carries the optional trace logger DAC writes when tracing is on.
// A trace reports request, retry, range, and cache decisions.
// Nothing here is part of the output contract.
package debug

import (
	"io"
	"log/slog"
)

// Discard is the logger a component uses when nothing turned tracing on.
var Discard = slog.New(slog.DiscardHandler)

// New returns the logger for one run: a trace to writer, or Discard.
func New(writer io.Writer, enabled bool) *slog.Logger {
	if !enabled {
		return Discard
	}
	return slog.New(slog.NewTextHandler(writer, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// Or returns logger, or Discard when there is none.
func Or(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return Discard
	}
	return logger
}
