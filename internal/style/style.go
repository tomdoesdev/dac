// Package style colours human-readable output.
//
// A command result is a sentence with a few things in it worth finding: which
// asset, which digest, and whether what it reports needs somebody to do
// something about it. Colour says that without spending words on it, and it is
// the one part of DAC's output allowed to carry no information at all -- the
// same text with the escape sequences taken back out has to read exactly as it
// did before, because most of the streams DAC writes to are not terminals and
// one of them is a parsed contract.
//
// So a palette is built for one writer rather than for the process. Standard
// output and standard error are different destinations and routinely different
// kinds of destination: `dac pull > log` leaves a terminal on one and a file on
// the other, and a single decision for the process would colour the file to
// match the terminal.
//
// What a terminal can render is termenv's problem rather than DAC's. It reads
// NO_COLOR, CLICOLOR, CLICOLOR_FORCE, CI, and TERM, and asks the file descriptor
// whether it is a terminal at all. Every one of those is a convention somebody
// expects DAC to already follow, they are the sort of thing that is wrong in a
// way nobody reports -- output arrives full of escape sequences in a log
// somewhere -- and none of them is worth reimplementing badly.
package style

import (
	"fmt"
	"io"
	"strings"

	"github.com/muesli/termenv"
)

// Mode is what a run was told to do about colour.
type Mode int

const (
	// Auto colours what a terminal will render and leaves everything else
	// alone. It is the default because a pipe is the common case and a pipe
	// full of escape sequences is the thing colour must never cost.
	Auto Mode = iota
	// Always colours regardless of the destination, for output that reaches a
	// terminal by way of something that is not one: a pager, a CI log viewer,
	// a file somebody will cat later.
	Always
	// Never colours nothing.
	Never
)

// ParseMode reads a --color value.
func ParseMode(value string) (Mode, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "auto":
		return Auto, nil
	case "always":
		return Always, nil
	case "never":
		return Never, nil
	}
	return Auto, fmt.Errorf("color %q is not auto, always, or never", value)
}

// Palette styles one stream of human-readable output.
//
// Its zero value colours nothing, which is the reading that fails safe: text
// nobody built a palette for should be the text somebody wrote. It carries the
// decision separately from the profile because termenv spells its richest
// profile as the zero of its own type, so a palette holding only a profile
// would light up every stream that got one by accident.
type Palette struct {
	colour  bool
	profile termenv.Profile
}

// New returns the palette for one writer.
func New(writer io.Writer, mode Mode) Palette {
	switch mode {
	case Never:
		return Palette{}
	case Always:
		// Forced colour cannot ask the destination what it can render, because
		// the usual reason to force it is that the destination is a pipe into
		// something that will render it later. Four-bit colour is what every
		// terminal had before any of them had more.
		return Palette{colour: true, profile: termenv.ANSI}
	}
	profile := termenv.NewOutput(writer).Profile
	return Palette{colour: profile != termenv.Ascii, profile: profile}
}

// Enabled reports whether this palette colours anything, for the callers that
// have to decide something rather than style something.
func (palette Palette) Enabled() bool { return palette.colour }

// The looks below are the terminal's own sixteen colours rather than exact
// values. DAC does not know what background it is being read on, and a green it
// picked itself would be the one unreadable colour on somebody's theme; a
// terminal's palette is a decision its owner already made.
//
// None of them nest. A look ends by resetting every attribute, so a name
// styled inside a warning would leave the rest of the warning unstyled, and
// each span of text gets exactly one.

// Strong marks what a line is actually about: the coordinate an info block
// describes, the count a summary was run for.
func (palette Palette) Strong(text string) string {
	return palette.paint(text, termenv.Style.Bold)
}

// Name marks a coordinate, which is the word somebody scanning a screen of
// results is scanning for.
func (palette Palette) Name(text string) string {
	return palette.paint(text, foreground(termenv.ANSICyan))
}

// Detail marks what is true but secondary: a digest, a timestamp, the key in
// front of a value. Digests are most of a cache listing by width and almost
// none of it by interest.
func (palette Palette) Detail(text string) string {
	return palette.paint(text, termenv.Style.Faint)
}

// Good marks a state that needs nothing from anybody.
func (palette Palette) Good(text string) string {
	return palette.paint(text, foreground(termenv.ANSIGreen))
}

// Warn marks a state somebody has to act on that is not a failure: an asset the
// cache does not hold, a lock file the manifest has outrun, the command that
// would settle either.
func (palette Palette) Warn(text string) string {
	return palette.paint(text, foreground(termenv.ANSIYellow))
}

// Bad marks a failure.
func (palette Palette) Bad(text string) string {
	return palette.paint(text, func(style termenv.Style) termenv.Style {
		return style.Foreground(termenv.ANSIRed).Bold()
	})
}

// paint applies one look, and hands the text back untouched when this palette
// colours nothing. Empty text is left alone too: a sequence around nothing is
// a sequence a terminal still has to interpret.
func (palette Palette) paint(text string, look func(termenv.Style) termenv.Style) string {
	if !palette.colour || text == "" {
		return text
	}
	return look(palette.profile.String(text)).String()
}

// foreground builds the look that sets one colour.
func foreground(colour termenv.ANSIColor) func(termenv.Style) termenv.Style {
	return func(style termenv.Style) termenv.Style { return style.Foreground(colour) }
}
