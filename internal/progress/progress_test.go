package progress

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestLineReporterWritesStableLifecycle(t *testing.T) {
	var output bytes.Buffer
	reporter := New(&output, false, true)
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
	reporter := New(&output, false, true)
	reporter.Fail("geo", errors.New("unavailable"))
	if output.String() != "fail geo unavailable\n" {
		t.Fatalf("unexpected progress: %q", output.String())
	}
}

func TestTerminalReporterUsesInjectedConsoleWriter(t *testing.T) {
	var output bytes.Buffer
	reporter := New(&output, true, true)
	reporter.Start("geo", 4)
	reporter.Advance("geo", 4)
	reporter.Done("geo", "downloaded")
	reporter.Wait()
	rendered := output.String()
	if !strings.Contains(rendered, "geo") || !strings.Contains(rendered, "downloaded") {
		t.Fatalf("terminal progress omitted state: %q", rendered)
	}
}

func TestTerminalReporterCompletesUnknownSizeSpinner(t *testing.T) {
	var output bytes.Buffer
	reporter := New(&output, true, true)
	reporter.Start("geo", -1)
	reporter.Advance("geo", 4)
	reporter.Done("geo", "resolved")
	reporter.Wait()
	rendered := output.String()
	if !strings.Contains(rendered, "geo") || !strings.Contains(rendered, "resolved") {
		t.Fatalf("spinner omitted state: %q", rendered)
	}
}

func TestDisabledReporterWritesNothing(t *testing.T) {
	var output bytes.Buffer
	reporter := New(&output, true, false)
	reporter.Start("geo", 4)
	reporter.Advance("geo", 4)
	reporter.Done("geo", "downloaded")
	reporter.Wait()
	if output.Len() != 0 {
		t.Fatalf("disabled progress wrote %q", output.String())
	}
}
