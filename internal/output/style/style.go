// Package style colours human-readable output.
// A palette highlights asset identity, digest detail, and states that need action.
// So a palette is built for one writer rather than for the process.
// What a terminal can render is termenv's problem rather than DAC's.
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
	// Auto colours what a terminal will render and leaves everything else alone.
	Auto Mode = iota
	// Always forces color for output that a later pager or viewer will render.
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
// Its zero value colours nothing, which is the reading that fails safe: text nobody built a palette for should be the text somebody wrote.
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
		// Forced color cannot inspect a pipe that a later process will render.
		return Palette{colour: true, profile: termenv.ANSI}
	}
	profile := termenv.NewOutput(writer).Profile
	return Palette{colour: profile != termenv.Ascii, profile: profile}
}

// Enabled reports whether this palette colours anything, for the callers that have to decide something rather than style something.
func (palette Palette) Enabled() bool { return palette.colour }

// The looks below are the terminal's own sixteen colours rather than exact values.
// None of them nest.

// Strong marks what a line is actually about: the coordinate an info block describes, the count a summary was run for.
func (palette Palette) Strong(text string) string {
	return palette.paint(text, termenv.Style.Bold)
}

// Name marks a coordinate, which is the word somebody scanning a screen of results is scanning for.
func (palette Palette) Name(text string) string {
	return palette.paint(text, foreground(termenv.ANSICyan))
}

// Detail marks what is true but secondary: a digest, a timestamp, the key in front of a value.
func (palette Palette) Detail(text string) string {
	return palette.paint(text, termenv.Style.Faint)
}

// Good marks a state that needs nothing from anybody.
func (palette Palette) Good(text string) string {
	return palette.paint(text, foreground(termenv.ANSIGreen))
}

// Warn marks actionable states that are not command failures.
func (palette Palette) Warn(text string) string {
	return palette.paint(text, foreground(termenv.ANSIYellow))
}

// Bad marks a failure.
func (palette Palette) Bad(text string) string {
	return palette.paint(text, func(style termenv.Style) termenv.Style {
		return style.Foreground(termenv.ANSIRed).Bold()
	})
}

// paint applies one look, and hands the text back untouched when this palette colours nothing.
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
