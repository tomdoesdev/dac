package progress

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/style"
)

func TestLineReporterWritesStableLifecycle(t *testing.T) {
	var output bytes.Buffer
	reporter := New(t.Context(), &output, false, true, style.Palette{})
	reporter.Start("geo", 4)
	reporter.Advance("geo", 4)
	reporter.Done("geo", "downloaded")
	reporter.Wait()
	if output.String() != "start geo 4 bytes\ndone geo downloaded\n" {
		t.Fatalf("unexpected progress: %q", output.String())
	}
}

func TestLineReporterWritesFailure(t *testing.T) {
	var output bytes.Buffer
	reporter := New(t.Context(), &output, false, true, style.Palette{})
	reporter.Fail("geo", errors.New("unavailable"))
	if output.String() != "fail geo unavailable\n" {
		t.Fatalf("unexpected progress: %q", output.String())
	}
}

func TestTerminalReporterUsesInjectedConsoleWriter(t *testing.T) {
	var output bytes.Buffer
	reporter := New(t.Context(), &output, true, true, style.Palette{})
	reporter.Start("geo", 4)
	reporter.Advance("geo", 4)
	reporter.Done("geo", "downloaded")
	reporter.Wait()
	rendered := output.String()
	if !strings.Contains(rendered, "geo") || !strings.Contains(rendered, "downloaded") {
		t.Fatalf("terminal progress omitted state: %q", rendered)
	}
}

// A bar is drawn into a fixed width, so anything added to it that the terminal
// does not draw has to be added where mpb knows not to count it. What this
// guards is that the name and the state survive being coloured at all: the
// alignment they would lose is not visible in a buffer, but an empty bar is.
func TestTerminalReporterColoursWithoutLosingTheText(t *testing.T) {
	var output bytes.Buffer
	reporter := New(t.Context(), &output, true, true, style.New(&output, style.Always))
	reporter.Start("geo", 4)
	reporter.Advance("geo", 4)
	reporter.Done("geo", "downloaded")
	reporter.Wait()
	rendered := output.String()
	if !strings.Contains(rendered, "\x1b[") {
		t.Fatalf("a forced palette drew no colour: %q", rendered)
	}
	plain := regexp.MustCompile("\x1b\\[[0-9;]*m").ReplaceAllString(rendered, "")
	if !strings.Contains(plain, "geo") || !strings.Contains(plain, "downloaded") {
		t.Fatalf("colour swallowed the state: %q", plain)
	}
}

func TestTerminalReporterCompletesUnknownSizeSpinner(t *testing.T) {
	var output bytes.Buffer
	reporter := New(t.Context(), &output, true, true, style.Palette{})
	reporter.Start("geo", -1)
	reporter.Advance("geo", 4)
	reporter.Done("geo", "resolved")
	reporter.Wait()
	rendered := output.String()
	if !strings.Contains(rendered, "geo") || !strings.Contains(rendered, "resolved") {
		t.Fatalf("spinner omitted state: %q", rendered)
	}
}

// An interrupt cancels the command context while transfers are still running,
// and the commands report nothing for an asset cut short that way. The bars
// those transfers started are therefore never finished by hand, so cancellation
// has to finish them: a Wait that blocks here is a DAC that Ctrl+C cannot stop.
func TestTerminalReporterStopsWaitingOnCancellation(t *testing.T) {
	var output bytes.Buffer
	ctx, cancel := context.WithCancel(t.Context())
	reporter := New(ctx, &output, true, true, style.Palette{})
	reporter.Start("geo", 1024)
	reporter.Advance("geo", 16)
	reporter.Start("tiles", 2048)
	cancel()

	returned := make(chan struct{})
	go func() {
		reporter.Wait()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait blocked on the bars an interrupt left unfinished")
	}
}

func TestDisabledReporterWritesNothing(t *testing.T) {
	var output bytes.Buffer
	reporter := New(t.Context(), &output, true, false, style.Palette{})
	reporter.Start("geo", 4)
	reporter.Advance("geo", 4)
	reporter.Done("geo", "downloaded")
	reporter.Wait()
	if output.Len() != 0 {
		t.Fatalf("disabled progress wrote %q", output.String())
	}
}
