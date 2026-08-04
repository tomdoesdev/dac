// Package debug carries the optional trace logger DAC writes when tracing is on.
//
// What a transfer did was invisible. DAC is pointed at a site's mirror by a
// rewrite rule, handed credentials by a program it starts, and told to retry and
// to split downloads across range requests -- and when any of that misbehaves,
// the only thing on screen is the failure at the end of it. dac info answers the
// rewrite question and nothing answers the rest: which URL was actually
// requested, whether the split path engaged and over how many parts, which
// helper answered, how many retries happened and what each one saw, whether an
// object came from the cache or the origin.
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
