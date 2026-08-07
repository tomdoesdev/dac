package progress

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/vbauerster/mpb/v8"

	"github.com/tomdoesdev/dac/internal/output/style"
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

// The first asset to fail cancels the transfers running beside it, and those report nothing, so their bars are left as unfinished as an interrupt leaves them.
// The difference is that nobody cancelled the command context, so nothing outside the reporter is going to end them: a pull whose origin does not resolve waits for good on a display that has stopped moving.
func TestTerminalReporterStopsWaitingOnBarsAFailureCutShort(t *testing.T) {
	var output bytes.Buffer
	reporter := New(t.Context(), &output, true, true, style.Palette{})
	reporter.Start("geo", 1024)
	reporter.Advance("geo", 16)
	reporter.Start("tiles", 2048)
	// geo could not be reached, and tiles was cancelled by that and says nothing.
	reporter.Fail("geo", errors.New("dial tcp: lookup geo.invalid: no such host"))

	returned := make(chan struct{})
	go func() {
		reporter.Wait()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait blocked on the bar another asset's failure cut short")
	}
	if !strings.Contains(output.String(), "failed") {
		t.Fatalf("the failure that ended the run was not drawn: %q", output.String())
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

// The rest of this file is about the one property a screen of bars has to have
// and used to lose: a column is in the same place on every row, and it stays
// there while the text in it changes.
//
// What is asserted is the drawn rows themselves rather than any of the widths
// that produce them, because the widths were never the thing that was wrong.
// mpb re-measured every column on every frame against whatever was in it, so
// the display slid about as a transfer crossed from KiB to MiB, as
// "downloading" settled into "cached", and as a finished bar left the group
// being measured -- all of which the scenarios below do on purpose.

// screen collects what a terminal of the given width would have been shown.
type screen struct {
	mutex sync.Mutex
	rows  []string
}

var escapes = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]")

func (output *screen) Write(frame []byte) (int, error) {
	output.mutex.Lock()
	defer output.mutex.Unlock()
	for line := range strings.SplitSeq(escapes.ReplaceAllString(string(frame), ""), "\n") {
		if line = strings.Trim(line, "\r"); strings.TrimSpace(line) != "" {
			output.rows = append(output.rows, line)
		}
	}
	return len(frame), nil
}

// draw runs one scenario against a terminal of a fixed width and returns every
// row it drew, over every frame. A real terminal answers for its own width, so
// the container is built here rather than by New.
func draw(t *testing.T, width int, scenario func(reporter *bars)) []string {
	t.Helper()
	output := &screen{}
	reporter := &bars{
		output: mpb.NewWithContext(t.Context(), mpb.WithOutput(output), mpb.ForceAutoRefresh(),
			mpb.PopCompletedMode(), mpb.WithWidth(width)),
		entries: map[string]*barState{},
	}
	scenario(reporter)
	reporter.Wait()
	output.mutex.Lock()
	defer output.mutex.Unlock()
	if len(output.rows) == 0 {
		t.Fatal("the reporter drew nothing")
	}
	return output.rows
}

// at is where a mark falls on a row, counted in the columns a terminal draws
// rather than in the bytes a Go string holds. The two differ on exactly the
// rows this file is about: a clipped name ends in an ellipsis and a spinner is
// a braille glyph, and both are three bytes wide and one column wide.
func at(row string, mark rune) int {
	found := strings.IndexRune(row, mark)
	if found < 0 {
		return -1
	}
	return utf8.RuneCountInString(row[:found])
}

// transfers is the scenario the columns have to survive: names of different
// lengths, sizes three units apart, an asset with no declared size at all, and
// four different endings arriving at four different times.
func transfers(reporter *bars) {
	names := []string{"datasets/geo-tiles@1.4.0", "models/embeddings@0.2.1", "fixtures/tiny@1", "datasets/atlas@2"}
	reporter.Plan(names)
	reporter.Start(names[0], 9_800_000)
	reporter.Start(names[1], 1_400_000_000)
	reporter.Start(names[2], 812)
	reporter.Start(names[3], -1)
	for range 3 {
		reporter.Advance(names[0], 700_000)
		reporter.Advance(names[1], 120_000_000)
		reporter.Advance(names[3], 33_000)
		time.Sleep(60 * time.Millisecond)
	}
	reporter.Done(names[2], "cached")
	time.Sleep(160 * time.Millisecond)
	reporter.Done(names[3], "not_modified")
	reporter.Advance(names[0], 5_600_000)
	reporter.Done(names[0], "downloaded")
	reporter.Fail(names[1], errors.New("unavailable"))
}

func TestTerminalReporterDrawsEveryRowToTheSameWidth(t *testing.T) {
	for _, width := range []int{80, 100, 120, 160} {
		rows := draw(t, width, transfers)
		for _, row := range rows {
			if drawn := utf8.RuneCountInString(row); drawn != utf8.RuneCountInString(rows[0]) {
				t.Fatalf("terminal of %d columns drew rows of %d and %d:\n%s",
					width, utf8.RuneCountInString(rows[0]), drawn, strings.Join(rows, "\n"))
			}
			if drawn := utf8.RuneCountInString(row); drawn > width {
				t.Fatalf("a row of %d ran off a terminal of %d columns: %q", drawn, width, row)
			}
		}
	}
}

// The bar is what mpb draws last, out of whatever the decorators left it, so it
// is the column that goes first when anything else takes more room than it did
// the frame before.
func TestTerminalReporterDrawsTheBarInTheSamePlaceAtItsFullWidth(t *testing.T) {
	for _, width := range []int{80, 100, 120, 160} {
		rows := draw(t, width, transfers)
		start := -1
		for _, row := range rows {
			opened := at(row, '[')
			if opened < 0 {
				continue // a spinner: it has a column but no brackets.
			}
			if start < 0 {
				start = opened
			}
			closed := at(row, ']')
			switch {
			case opened != start:
				t.Fatalf("terminal of %d columns moved the bar from column %d to %d:\n%s",
					width, start, opened, strings.Join(rows, "\n"))
			case closed-opened+1 != barWidth:
				t.Fatalf("terminal of %d columns drew a bar %d wide, not %d: %q",
					width, closed-opened+1, barWidth, row)
			}
		}
		if start < 0 {
			t.Fatalf("terminal of %d columns drew no bar at all", width)
		}
	}
}

// A percentage is the column that changes width most often and by the most: a
// transfer spends one frame at "0%" and its last at "100%".
func TestTerminalReporterHoldsThePercentageColumnAsItFills(t *testing.T) {
	rows := draw(t, 120, transfers)
	end := -1
	for _, row := range rows {
		found := at(row, '%')
		if found < 0 {
			continue // an asset with no declared size has no percentage.
		}
		if end < 0 {
			end = found
		}
		if found != end {
			t.Fatalf("the percentage moved from column %d to %d:\n%s", end, found, strings.Join(rows, "\n"))
		}
	}
	if end < 0 {
		t.Fatal("no percentage was drawn")
	}
}

// A name column measured from the bars already started would widen part way
// through a pull, and the rows PopCompletedMode has printed above the display
// cannot be redrawn to match.
func TestPlanSizesTheNameColumnBeforeTheFirstBar(t *testing.T) {
	short, long := "a/b@1", "datasets/geo-tiles-elevation@1.4.0"
	planned := draw(t, 120, func(reporter *bars) {
		reporter.Plan([]string{short, long})
		reporter.Start(short, 512)
		reporter.Advance(short, 512)
		reporter.Done(short, "cached")
	})
	unplanned := draw(t, 120, func(reporter *bars) {
		reporter.Start(short, 512)
		reporter.Advance(short, 512)
		reporter.Done(short, "cached")
	})
	gap := at(planned[0], '[') - at(unplanned[0], '[')
	if want := utf8.RuneCountInString(long) - utf8.RuneCountInString(short); gap != want {
		t.Fatalf("planning a %d character name widened the column by %d, not %d:\n%q\n%q",
			utf8.RuneCountInString(long), gap, want, planned[0], unplanned[0])
	}
}

// A name nothing can be done about is cut rather than allowed to take the line,
// and the cut is marked so that nobody reads what is left as a whole coordinate.
func TestTerminalReporterClipsANameTooLongForItsColumn(t *testing.T) {
	name := strings.Repeat("very-long-namespace/", 4) + "asset@1.0.0"
	rows := draw(t, 120, func(reporter *bars) {
		reporter.Plan([]string{name})
		reporter.Start(name, 512)
		reporter.Advance(name, 512)
		reporter.Done(name, "cached")
	})
	if opened := at(rows[0], '['); opened != nameLimit+2 {
		t.Fatalf("a %d character name put the bar at column %d, not %d: %q",
			utf8.RuneCountInString(name), opened, nameLimit+2, rows[0])
	}
	if !strings.Contains(rows[0], "…") {
		t.Fatalf("a clipped name was not marked as clipped: %q", rows[0])
	}
}

// An asset answered from the cache moved no bytes, and a rate reported for it
// would be the whole of it in no time at all.
func TestTerminalReporterReportsNoSpeedForATransferThatNeverRan(t *testing.T) {
	rows := draw(t, 160, func(reporter *bars) {
		reporter.Start("fixtures/tiny@1", 4096)
		reporter.Done("fixtures/tiny@1", "cached")
	})
	if strings.Contains(rows[0], "/s") {
		t.Fatalf("a cached asset was given a transfer speed: %q", rows[0])
	}
}
