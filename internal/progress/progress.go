// Package progress writes asset transfer progress to standard error.
package progress

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/bytesize"
	"github.com/tomdoesdev/dac/internal/style"
)

// New selects bars for a terminal and lines for other writers.
// The context is the command's, and the bar container is built on it so that cancelling the command ends the display.
// Only the bars are coloured.
func New(ctx context.Context, writer io.Writer, terminal, enabled bool, palette style.Palette) application.Reporter {
	if !enabled {
		return application.NopReporter{}
	}
	if terminal {
		return newBars(ctx, writer, palette)
	}
	return &lines{writer: writer}
}

type lines struct {
	mutex  sync.Mutex
	writer io.Writer
}

// Plan is nothing to a reporter with no columns to lay out.
func (*lines) Plan([]string) {}

func (reporter *lines) Start(name string, total int64) {
	reporter.write("start %s %d bytes\n", name, total)
}

func (*lines) Advance(string, int64) {}

func (reporter *lines) Done(name, status string) {
	reporter.write("done %s %s\n", name, status)
}

func (reporter *lines) Fail(name string, err error) {
	reporter.write("fail %s %s\n", name, err)
}

func (*lines) Wait() {}

func (reporter *lines) write(format string, values ...any) {
	reporter.mutex.Lock()
	defer reporter.mutex.Unlock()
	_, _ = fmt.Fprintf(reporter.writer, format, values...)
}

// A screen of bars is a table, and the thing that makes it one is that a column is in the same place on every row and stays there.
// mpb offers to arrange that itself: a decorator marked WCSyncSpace is drawn to the width of the widest cell in its column.
// The widths below are DAC's instead.
const (
	// barWidth is the drawn width of the bar itself, brackets included.
	barWidth = 24
	// nameLimit bounds the name column.
	nameLimit = 36
	// sizeWidth fits the widest number bytesize.Format writes.
	sizeWidth = 6
	// unitWidth fits "KiB", and rateUnitWidth the same unit as a rate.
	unitWidth     = 3
	rateUnitWidth = unitWidth + len("/s")
	// percentWidth fits "100%".
	percentWidth = 4
	// statusWidth is what the status column is reserved at.
	statusWidth = 12
	// gutter separates one column from the next.
	gutter = "  "
)

// amountWidth and rateWidth are what amount and rate draw into: a number, a space, and a unit.
const (
	amountWidth = sizeWidth + 1 + unitWidth
	rateWidth   = sizeWidth + 1 + rateUnitWidth

	percentColumn  = 1 + percentWidth
	countersColumn = len(gutter) + amountWidth + len(" / ") + amountWidth
	speedColumn    = len(gutter) + rateWidth
	statusColumn   = len(gutter) + statusWidth
)

// essentialWidth reserves the fixed bar and status columns beside the asset name.
// It is what the name column is measured against, because the name is the one column with no width of its own to hold it back.
const (
	essentialWidth = 3 + barWidth + percentColumn + statusColumn
	// minNameWidth is what the name column keeps on a terminal too narrow for even that.
	minNameWidth = 8
)

type bars struct {
	mutex   sync.Mutex
	output  *mpb.Progress
	entries map[string]*barState
	palette style.Palette
	// name is the width of the name column, read by every bar on every frame and only ever widened.
	name atomic.Int64
}

type barState struct {
	mutex  sync.Mutex
	bar    *mpb.Bar
	total  int64
	status string
	// settled records that this transfer reached an outcome, which is what Wait takes a bar off the display for the absence of.
	// It is not the same question as whether mpb still considers the bar running: an aborted bar has settled, and a bar the command stopped touching has not.
	settled bool
	// look colours the status word, and is nil while there is nothing to say about it.
	look func(string) string
	// start and elapsed bound the window the speed column reports over.
	start   time.Time
	elapsed time.Duration
	// moved excludes cached bytes that a completed bar includes in its current count.
	moved atomic.Int64
}

// newBars builds the bar container on the command's context.
// Wait ends active bars and omits assets that the command context canceled.
func newBars(ctx context.Context, writer io.Writer, palette style.Palette) *bars {
	return &bars{
		output:  mpb.NewWithContext(ctx, mpb.WithOutput(writer), mpb.ForceAutoRefresh(), mpb.PopCompletedMode()),
		entries: map[string]*barState{},
		palette: palette,
	}
}

// Plan sizes the name column for the assets a command is about to work through.
// Every other column has a width DAC can state in advance; this one is as wide as the longest name it will hold, and a name is not known until an asset starts.
// Plan fixes name width before later assets can move the display.
func (reporter *bars) Plan(names []string) {
	for _, name := range names {
		reporter.widen(name)
	}
}

func (reporter *bars) Start(name string, total int64) {
	reporter.widen(name)
	reporter.mutex.Lock()
	defer reporter.mutex.Unlock()
	if _, exists := reporter.entries[name]; exists {
		return
	}
	state := &barState{total: total, status: "downloading", start: time.Now()}
	// A padded status keeps bar widths equal across rows.
	status := decor.Any(func(decor.Statistics) string {
		state.mutex.Lock()
		defer state.mutex.Unlock()
		return pad(state.status, statusWidth, false)
	})
	status = reporter.meta(status, func(text string) string {
		state.mutex.Lock()
		defer state.mutex.Unlock()
		if state.look == nil {
			return text
		}
		return state.look(text)
	})
	appenders := []decor.Decorator{
		// mpb writes one space between the bar and this group, and the columns carry the other one each.
		decor.Any(func(stat decor.Statistics) string {
			return " " + percentage(stat.Current, total)
		}),
		// Optional byte and speed columns render together because decorators cannot inspect earlier output.
		decor.Any(func(stat decor.Statistics) string {
			if !fits(stat, countersColumn) {
				return ""
			}
			line := gutter + counters(stat.Current, total)
			if fits(stat, countersColumn+speedColumn) {
				line += gutter + rate(state.moved.Load(), state.window())
			}
			return line
		}),
		// The gutter in front of the status is drawn on its own so that what the palette colours is the word and not the space before it.
		decor.Any(func(decor.Statistics) string { return gutter }),
		// Status renders last because service values can exceed the expected width.
		status,
	}
	options := []mpb.BarOption{
		mpb.BarWidth(barWidth),
		mpb.PrependDecorators(reporter.meta(decor.Any(func(stat decor.Statistics) string {
			width := reporter.nameWidth(stat)
			return pad(clip(name, width), width, false) + " "
		}), reporter.palette.Name)),
		mpb.AppendDecorators(appenders...),
	}
	if total >= 0 {
		state.bar = reporter.output.New(0, reporter.barStyle(), options...)
		state.bar.SetTotal(total, false)
	} else {
		state.bar = reporter.output.AddSpinner(0, options...)
	}
	reporter.entries[name] = state
}

func (reporter *bars) Advance(name string, count int64) {
	if state := reporter.entry(name); state != nil {
		state.moved.Add(count)
		state.bar.IncrInt64(count)
	}
}

func (reporter *bars) Done(name, status string) {
	state := reporter.entry(name)
	if state == nil {
		_, _ = fmt.Fprintf(reporter.output, "done %s %s\n", name, status)
		return
	}
	state.settle(status, reporter.palette.Good)
	if state.total >= 0 {
		state.bar.SetTotal(state.total, true)
	} else {
		state.bar.SetTotal(-1, true)
	}
}

func (reporter *bars) Fail(name string, err error) {
	state := reporter.entry(name)
	if state == nil {
		_, _ = fmt.Fprintf(reporter.output, "fail %s %s\n", name, err)
		return
	}
	state.settle("failed", reporter.palette.Bad)
	state.bar.Abort(false)
}

// Wait ends the display and blocks until it has finished drawing.
//
// It takes down any bar the command left running, because mpb waits for every bar it was given and a command does not settle every bar it starts.
// The first asset to fail cancels the rest, and an asset cut short that way is reported as nothing at all: the only message it could carry would describe another asset's failure.
// That is the right thing to say and the wrong thing to leave on screen, and a bar nobody was going to finish held the whole command open behind a display that had stopped moving.
//
// The bars come off rather than standing, because a row still reading "downloading" for a transfer that has stopped is a worse answer than no row, and the failure that ended the run prints straight after this returns.
// Every operation calls this once its transfers have finished, so a bar still unsettled here is one nothing was ever going to settle.
func (reporter *bars) Wait() {
	reporter.mutex.Lock()
	for _, state := range reporter.entries {
		if !state.done() {
			state.bar.Abort(true)
		}
	}
	reporter.mutex.Unlock()
	reporter.output.Wait()
}

// settle records what became of a transfer and stops its clock.
func (state *barState) settle(status string, look func(string) string) {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	state.status, state.look = status, look
	state.settled = true
	if state.elapsed == 0 {
		state.elapsed = time.Since(state.start)
	}
}

// done reports whether this transfer reached an outcome.
func (state *barState) done() bool {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	return state.settled
}

// window is how long this transfer has been running, or how long it ran.
func (state *barState) window() time.Duration {
	state.mutex.Lock()
	defer state.mutex.Unlock()
	if state.elapsed > 0 {
		return state.elapsed
	}
	return time.Since(state.start)
}

// nameWidth fits the longest asset name beside the required columns.
// It is read from the first decorator on the line, where mpb has not yet taken anything off the width it reports, so what it sees is the terminal itself.
func (reporter *bars) nameWidth(stat decor.Statistics) int {
	width := min(int(reporter.name.Load()), nameLimit)
	if room := stat.AvailableWidth - essentialWidth; room < width {
		return max(room, minNameWidth)
	}
	return width
}

// widen grows the name column to hold one more name.
func (reporter *bars) widen(name string) {
	width := int64(min(utf8.RuneCountInString(name), nameLimit))
	for {
		current := reporter.name.Load()
		if width <= current || reporter.name.CompareAndSwap(current, width) {
			return
		}
	}
}

// meta applies one look to a decorator.
// It is mpb's answer to a string that got longer without getting wider: the decorator reports its own width, and the escape sequences go on afterwards.
// An uncolored reporter returns text directly and avoids work on every redraw.
func (reporter *bars) meta(decorator decor.Decorator, look func(string) string) decor.Decorator {
	if !reporter.palette.Enabled() {
		return decorator
	}
	return decor.Meta(decorator, look)
}

// barStyle colours the bar itself: the part that is done takes the accent the asset names take, and the part still to go recedes.
func (reporter *bars) barStyle() mpb.BarStyleComposer {
	if !reporter.palette.Enabled() {
		return mpb.BarStyle()
	}
	return mpb.BarStyle().
		FillerMeta(reporter.palette.Name).
		TipMeta(reporter.palette.Name).
		PaddingMeta(reporter.palette.Detail)
}

func (reporter *bars) entry(name string) *barState {
	reporter.mutex.Lock()
	defer reporter.mutex.Unlock()
	return reporter.entries[name]
}

// fits reports whether a column of this width still leaves the line room for the bar and for the status behind it.
// mpb draws the decorators first and gives the bar whatever they left, then cuts the line at the terminal's edge if they left nothing.
// Every bar spends column widths in one order so rows align in each frame.
func fits(stat decor.Statistics, column int) bool {
	// The two spaces mpb writes around the bar are subtracted after the decorators have run, so they are still owed here.
	return stat.AvailableWidth-barWidth-2-statusColumn >= column
}

// counters draws how much of an asset has arrived against how much of it there is, as two amounts in fixed columns.
// An unknown total aligns its byte count with the total column used by other rows.
func counters(current, total int64) string {
	if total < 0 {
		return blank(amountWidth+len(" / ")) + amount(current)
	}
	return amount(current) + " / " + amount(total)
}

// amount writes a byte count as a right-aligned number and a left-aligned unit,
// so that a column of them lines up on both:
//
//	 12.4 MiB
//	999.0 KiB
//	   64 B
//
// The spelling is bytesize's, which is the one every other DAC command writes,
// so a size read off a bar and the same size read out of a summary are the same
// text rather than two roundings of one number.
func amount(count int64) string {
	number, unit, _ := strings.Cut(bytesize.Format(count), " ")
	return pad(number, sizeWidth, true) + " " + pad(unit, unitWidth, false)
}

// rate writes a transfer speed into the same shape as an amount.
// A transfer with no bytes has no speed; zero would look like a stalled download.
func rate(moved int64, window time.Duration) string {
	if moved <= 0 || window <= 0 {
		return blank(rateWidth)
	}
	number, unit, _ := strings.Cut(bytesize.Format(int64(float64(moved)/window.Seconds())), " ")
	return pad(number, sizeWidth, true) + " " + pad(unit+"/s", rateUnitWidth, false)
}

// percentage draws how far along a transfer is, and nothing at all for one whose total nobody declared.
// An empty asset is complete because its current count equals its total.
func percentage(current, total int64) string {
	if total < 0 {
		return blank(percentWidth)
	}
	value := int64(100)
	if total > 0 {
		value = min(current*100/total, 100)
	}
	return pad(strconv.FormatInt(value, 10)+"%", percentWidth, true)
}

// pad returns text drawn into width, against the right edge of the column or the left.
func pad(text string, width int, right bool) string {
	space := width - utf8.RuneCountInString(text)
	if space <= 0 {
		return text
	}
	if right {
		return strings.Repeat(" ", space) + text
	}
	return text + strings.Repeat(" ", space)
}

// clip shortens a name too long for its column, marking the cut so that nobody reads what is left as the whole coordinate.
func clip(text string, width int) string {
	if utf8.RuneCountInString(text) <= width || width < 1 {
		return text
	}
	return string([]rune(text)[:width-1]) + "…"
}

// blank is a column with nothing in it.
func blank(width int) string { return strings.Repeat(" ", width) }
