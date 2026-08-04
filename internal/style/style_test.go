package style

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// sequences matches the SGR escapes a palette writes, so a test can ask what a
// terminal would actually show.
var sequences = regexp.MustCompile("\x1b\\[[0-9;]*m")

func plain(text string) string { return sequences.ReplaceAllString(text, "") }

// A palette nobody built is the one most likely to be reached by accident, and
// the stream behind it is as likely to be a pipe as a terminal.
func TestZeroPaletteColoursNothing(t *testing.T) {
	var palette Palette
	if palette.Enabled() {
		t.Fatal("the zero palette reports itself enabled")
	}
	for _, styled := range everyLook(palette, "app/geo@1") {
		if styled != "app/geo@1" {
			t.Fatalf("the zero palette styled text: %q", styled)
		}
	}
}

func TestNeverColoursNothingAndAlwaysColoursEverything(t *testing.T) {
	var buffer bytes.Buffer
	never := New(&buffer, Never)
	if never.Enabled() {
		t.Fatal("--color=never built an enabled palette")
	}
	for _, styled := range everyLook(never, "cached") {
		if styled != "cached" {
			t.Fatalf("--color=never styled text: %q", styled)
		}
	}

	always := New(&buffer, Always)
	if !always.Enabled() {
		t.Fatal("--color=always built a disabled palette")
	}
	for _, styled := range everyLook(always, "cached") {
		if !strings.HasPrefix(styled, "\x1b[") {
			t.Fatalf("--color=always left text unstyled: %q", styled)
		}
		// The whole point of colour is that it adds nothing a reader has to
		// read: the same text with the escapes taken back out is the text
		// somebody wrote.
		if plain(styled) != "cached" {
			t.Fatalf("a look changed the text: %q", styled)
		}
	}
}

// Auto is the mode nearly every run uses, and nearly every run is a pipe: a
// buffer is not a terminal, so nothing about it should end up coloured.
func TestAutoLeavesSomethingThatIsNotATerminalAlone(t *testing.T) {
	var buffer bytes.Buffer
	palette := New(&buffer, Auto)
	if palette.Enabled() {
		t.Fatal("auto coloured a writer that is not a terminal")
	}
	if styled := palette.Bad("Error:"); styled != "Error:" {
		t.Fatalf("auto styled text for a buffer: %q", styled)
	}
}

// An escape sequence around nothing is still a sequence the terminal reads, and
// every one of these looks is applied to a value that is sometimes absent.
func TestLooksLeaveEmptyTextEmpty(t *testing.T) {
	var buffer bytes.Buffer
	palette := New(&buffer, Always)
	for _, styled := range everyLook(palette, "") {
		if styled != "" {
			t.Fatalf("a look wrote %q for empty text", styled)
		}
	}
}

func TestParseModeReadsTheThreeValues(t *testing.T) {
	for value, want := range map[string]Mode{
		"auto": Auto, "always": Always, "never": Never,
		"ALWAYS": Always, " never ": Never,
	} {
		mode, err := ParseMode(value)
		if err != nil || mode != want {
			t.Fatalf("ParseMode(%q) = %v, %v", value, mode, err)
		}
	}
	// Including the empty string, which is what --color= writes. Somebody who
	// typed it meant something by it, and DAC does not get to decide what.
	for _, value := range []string{"", "yes", "alwyas", "1"} {
		if mode, err := ParseMode(value); err == nil {
			t.Fatalf("ParseMode(%q) accepted the value as %v", value, mode)
		} else if mode != Auto {
			t.Fatalf("ParseMode(%q) failed to a mode other than auto: %v", value, mode)
		}
	}
}

// everyLook applies each of the palette's looks to one string, for the
// properties that hold of all of them.
func everyLook(palette Palette, text string) []string {
	return []string{
		palette.Strong(text),
		palette.Name(text),
		palette.Detail(text),
		palette.Good(text),
		palette.Warn(text),
		palette.Bad(text),
	}
}
