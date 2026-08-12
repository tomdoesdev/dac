package cli

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
)

var ansiSequence = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// TestStylerColorModes covers every public semantic role through deterministic
// profiles so callers can rely on both ANSI and plain rendering.
func TestStylerColorModes(t *testing.T) {
	if got := new(Styler).Error("plain"); got != "plain" {
		t.Fatalf("zero-value style = %q", got)
	}

	roles := []struct {
		name   string
		ansi   string
		format func(*Styler, string) string
	}{
		{name: "heading", ansi: "\x1b[36;1mvalue\x1b[0m", format: (*Styler).Heading},
		{name: "command", ansi: "\x1b[36mvalue\x1b[0m", format: (*Styler).Command},
		{name: "flag", ansi: "\x1b[33mvalue\x1b[0m", format: (*Styler).Flag},
		{name: "argument", ansi: "\x1b[35mvalue\x1b[0m", format: (*Styler).Argument},
		{name: "progress", ansi: "\x1b[36mvalue\x1b[0m", format: (*Styler).Progress},
		{name: "success", ansi: "\x1b[32mvalue\x1b[0m", format: (*Styler).Success},
		{name: "warning", ansi: "\x1b[33mvalue\x1b[0m", format: (*Styler).Warning},
		{name: "error", ansi: "\x1b[31mvalue\x1b[0m", format: (*Styler).Error},
	}

	for _, role := range roles {
		t.Run(role.name, func(t *testing.T) {
			if got := role.format(NewStyler(&bytes.Buffer{}, ColorNever), "value"); got != "value" {
				t.Fatalf("disabled style = %q", got)
			}
			colored := role.format(NewStyler(&bytes.Buffer{}, ColorAlways), "value")
			if colored != role.ansi {
				t.Fatalf("forced style = %q, want %q", colored, role.ansi)
			}
		})
	}
}

// TestStylerAutoMode follows terminal conventions while keeping redirected
// output plain unless the environment explicitly forces color.
func TestStylerAutoMode(t *testing.T) {
	t.Run("redirected", func(t *testing.T) {
		t.Setenv("NO_COLOR", "")
		t.Setenv("CLICOLOR", "")
		t.Setenv("CLICOLOR_FORCE", "")
		if got := NewStyler(&bytes.Buffer{}, ColorAuto).Success("ok"); got != "ok" {
			t.Fatalf("redirected auto style = %q", got)
		}
	})

	t.Run("forced", func(t *testing.T) {
		t.Setenv("NO_COLOR", "")
		t.Setenv("CLICOLOR_FORCE", "1")
		if got := NewStyler(&bytes.Buffer{}, ColorAuto).Success("ok"); !strings.Contains(got, "\x1b[") {
			t.Fatalf("forced auto style = %q", got)
		}
	})

	t.Run("no-color", func(t *testing.T) {
		t.Setenv("NO_COLOR", "1")
		t.Setenv("CLICOLOR_FORCE", "")
		if got := NewStyler(&bytes.Buffer{}, ColorAuto).Success("ok"); got != "ok" {
			t.Fatalf("NO_COLOR auto style = %q", got)
		}
	})
}

// TestStylerRejectsInvalidConfiguration keeps construction failures immediate
// rather than allowing a bad mode or writer to surface much later in output.
func TestStylerRejectsInvalidConfiguration(t *testing.T) {
	for name, construct := range map[string]func(){
		"nil writer":   func() { NewStyler(nil, ColorAuto) },
		"invalid mode": func() { NewStyler(&bytes.Buffer{}, ColorMode(255)) },
		"app option":   func() { WithColor(ColorMode(255)) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid color configuration did not panic")
				}
			}()
			construct()
		})
	}
}

// TestAppColorRendering proves styling is presentation-only: stripping ANSI
// from forced help yields exactly the established plain rendering.
func TestAppColorRendering(t *testing.T) {
	render := func(mode ColorMode) string {
		output := &bytes.Buffer{}
		app := New(context.Background(), WithName("tool"), WithOutput(output), WithColor(mode))
		app.MustAddCommand("run <name>", &nameHandler{description: "Run a command"})
		if err := app.Run([]string{"run", "--help"}); err != nil {
			t.Fatal(err)
		}
		return output.String()
	}

	plain := render(ColorNever)
	colored := render(ColorAlways)
	if !strings.Contains(colored, "\x1b[") {
		t.Fatalf("forced help has no ANSI styling: %q", colored)
	}
	if stripped := ansiSequence.ReplaceAllString(colored, ""); stripped != plain {
		t.Fatalf("stripped help differs:\n got %q\nwant %q", stripped, plain)
	}
}

// TestUsageAndDeprecationUseErrorProfile checks both diagnostics owned by App.
// Error messages remain plain while usage and warning prefixes are styled.
func TestUsageAndDeprecationUseErrorProfile(t *testing.T) {
	usageApp := New(context.Background(), WithName("tool"), WithOutput(&bytes.Buffer{}), WithErrorOutput(&bytes.Buffer{}), WithColor(ColorAlways))
	usageApp.MustAddCommand("run", &minimalHandler{})
	err := usageApp.Run([]string{"run", "--unknown"})
	var usage *UsageError
	if !errors.As(err, &usage) || strings.Contains(usage.Error(), "\x1b[") || !strings.Contains(usage.Usage(), "\x1b[") {
		t.Fatalf("usage error = %#v, %v", usage, err)
	}

	warnings := &bytes.Buffer{}
	deprecatedApp := New(context.Background(), WithOutput(&bytes.Buffer{}), WithErrorOutput(warnings), WithColor(ColorAlways))
	deprecatedApp.MustAddCommand("old", &minimalHandler{}, WithDeprecated("use new"))
	if err := deprecatedApp.Run([]string{"old"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warnings.String(), "\x1b[") || ansiSequence.ReplaceAllString(warnings.String(), "") != "Warning: command \"old\" is deprecated: use new\n" {
		t.Fatalf("deprecation warning = %q", warnings.String())
	}
}

// TestMachineOutputNeverUsesColor prevents terminal configuration from leaking
// escape sequences into stable protocols and script-friendly output.
func TestMachineOutputNeverUsesColor(t *testing.T) {
	version := &bytes.Buffer{}
	if err := New(context.Background(), WithName("tool"), WithVersion("1.2.3"), WithOutput(version), WithColor(ColorAlways)).Run([]string{"--version"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(version.String(), "\x1b[") {
		t.Fatalf("version contains ANSI: %q", version.String())
	}

	script := &bytes.Buffer{}
	if err := New(context.Background(), WithName("tool"), WithCompletion(), WithOutput(script), WithColor(ColorAlways)).Run([]string{"completion", "bash"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script.String(), "\x1b[") {
		t.Fatalf("completion script contains ANSI: %q", script.String())
	}

	candidates := &bytes.Buffer{}
	app := New(context.Background(), WithCompletion(), WithOutput(candidates), WithColor(ColorAlways))
	app.MustAddCommand("remote add", &minimalHandler{})
	if err := app.Run([]string{"__complete", "r"}); err != nil {
		t.Fatal(err)
	}
	if candidates.String() != "remote\n" {
		t.Fatalf("completion candidates = %q", candidates.String())
	}
}

// TestColoredRowsPreserveUnicodeAndMultilineAlignment extends the plain layout
// invariant to styled labels, where escape-sequence bytes must be ignored.
func TestColoredRowsPreserveUnicodeAndMultilineAlignment(t *testing.T) {
	rows := []helpRow{
		{label: "é", help: "first line\nsecond line", role: helpCommand},
		{label: "ab", help: "other", role: helpCommand},
	}
	plain := renderRowsStyled(rows, NewStyler(&bytes.Buffer{}, ColorNever))
	colored := renderRowsStyled(rows, NewStyler(&bytes.Buffer{}, ColorAlways))
	if got := ansiSequence.ReplaceAllString(colored, ""); got != plain {
		t.Fatalf("colored rows differ after stripping ANSI: got %q, want %q", got, plain)
	}
}
