package cli

import (
	"io"

	"github.com/muesli/termenv"
)

// ColorMode controls when semantic CLI styles emit ANSI escape sequences.
type ColorMode uint8

const (
	// ColorAuto enables color when the writer is a capable terminal and honors
	// the NO_COLOR, CLICOLOR, and CLICOLOR_FORCE environment conventions.
	ColorAuto ColorMode = iota
	// ColorAlways emits portable ANSI color sequences regardless of the writer.
	ColorAlways
	// ColorNever renders every semantic style as plain text.
	ColorNever
)

// Styler formats text according to cli's stable semantic roles. Its internals
// deliberately hide the terminal library so callers depend only on cli's API.
// The zero value is safe and renders plain text; NewStyler applies a color mode.
type Styler struct {
	profile    termenv.Profile
	configured bool
}

// NewStyler creates a semantic formatter for output. Auto mode detects the
// writer's terminal capabilities and respects conventional color environment
// variables; deterministic modes are useful for configuration and tests.
func NewStyler(output io.Writer, mode ColorMode) *Styler {
	if output == nil {
		panic("cli: style output writer must not be nil")
	}

	var profile termenv.Profile
	switch mode {
	case ColorAuto:
		profile = termenv.NewOutput(output).Profile
	case ColorAlways:
		profile = termenv.ANSI
	case ColorNever:
		profile = termenv.Ascii
	default:
		panic("cli: invalid color mode")
	}
	return &Styler{profile: profile, configured: true}
}

// Heading emphasizes a generated output section heading.
func (styler *Styler) Heading(value string) string {
	if !styler.configured {
		return value
	}
	return styler.profile.String(value).Foreground(styler.profile.Color("6")).Bold().String()
}

// Command identifies an executable name or command path segment.
func (styler *Styler) Command(value string) string {
	return styler.foreground(value, "6")
}

// Flag identifies a command-line option.
func (styler *Styler) Flag(value string) string {
	return styler.foreground(value, "3")
}

// Argument identifies a positional argument or option value placeholder.
func (styler *Styler) Argument(value string) string {
	return styler.foreground(value, "5")
}

// Progress identifies transient activity such as a throbber glyph.
func (styler *Styler) Progress(value string) string {
	return styler.foreground(value, "6")
}

// Success identifies a successful action or healthy status.
func (styler *Styler) Success(value string) string {
	return styler.foreground(value, "2")
}

// Warning identifies a warning or unhealthy but recoverable status.
func (styler *Styler) Warning(value string) string {
	return styler.foreground(value, "3")
}

// Error identifies an error or failed status.
func (styler *Styler) Error(value string) string {
	return styler.foreground(value, "1")
}

// foreground applies one palette color while preserving plain output for the
// ASCII profile selected by disabled or unsupported color output.
func (styler *Styler) foreground(value, color string) string {
	if !styler.configured {
		return value
	}
	return styler.profile.String(value).Foreground(styler.profile.Color(color)).String()
}
