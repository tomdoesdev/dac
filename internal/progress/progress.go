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
//
// The context is the command's, and the bar container is built on it so that
// cancelling the command ends the display. See newBars: without it an interrupt
// leaves the process alive with nothing left to draw.
//
// Only the bars are coloured. The line form exists for the streams that are not
// terminals -- a CI log, a file, a pipe -- and it is one line per asset per
// event, which is a format something downstream is reading rather than a
// picture somebody is watching.
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

// Plan is nothing to a reporter with no columns to lay out. One line per asset
// per event says what happened when it happened, and a line already written is
// not moved by what a later one turns out to be.
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

// A screen of bars is a table, and the thing that makes it one is that a column
// is in the same place on every row and stays there.
//
// mpb offers to arrange that itself: a decorator marked WCSyncSpace is drawn to
// the width of the widest cell in its column. What it measures, though, is the
// widest cell in that column on the frame it is drawing, and every cell here
// changes -- a counter crosses from KiB to MiB and gains a character, a speed
// loses one, "downloading" settles into "cached", and PopCompletedMode takes a
// finished bar out of the set being measured entirely. So the columns were
// re-measured several times a second, every row moved whenever any row's
// longest cell changed, and finishing the widest-named asset pulled the whole
// display back to the left.
//
// The widths below are DAC's instead. They are the same on every frame, so a
// cell that changes changes only itself, and the ones with a bounded worst case
// -- a byte count, a percentage -- are wide enough for it up front. Only the
// two columns whose contents DAC does not bound need more than a number: the
// name column is sized from the names a command is about to report on (see
// Plan), and the status column is last on the line, where a word longer than
// expected has nothing to its right to push.
const (
	// barWidth is the drawn width of the bar itself, brackets included. A
	// spinner is drawn to the same width, so the two kinds of row line up.
	barWidth = 24
	// nameLimit bounds the name column. Each part of a coordinate is bounded at
	// 64 characters, so three of them and their separators could otherwise
	// reserve more than a terminal's width for the names alone.
	nameLimit = 36
	// sizeWidth fits the widest number bytesize.Format writes. A value moves up
	// to the next unit before it reaches 1024, so "1023.9" is as long as one
	// gets.
	sizeWidth = 6
	// unitWidth fits "KiB", and rateUnitWidth the same unit as a rate.
	unitWidth     = 3
	rateUnitWidth = unitWidth + len("/s")
	// percentWidth fits "100%".
	percentWidth = 4
	// statusWidth is what the status column is reserved at. It fits every word
	// the service reports, the longest of which is "not_modified". Nothing is
	// drawn to the right of it, so a longer one is written out in full and runs
	// off the end of the line rather than being cut or moving anything.
	statusWidth = 12
	// gutter separates one column from the next. mpb writes a space of its own
	// on each side of the bar, which is why the columns either side of it add
	// one rather than two.
	gutter = "  "
)

// amountWidth and rateWidth are what amount and rate draw into: a number, a
// space, and a unit. The column widths below are those plus the gutter in front
// of them, which is what a column costs the line.
const (
	amountWidth = sizeWidth + 1 + unitWidth
	rateWidth   = sizeWidth + 1 + rateUnitWidth

	percentColumn  = 1 + percentWidth
	countersColumn = len(gutter) + amountWidth + len(" / ") + amountWidth
	speedColumn    = len(gutter) + rateWidth
	statusColumn   = len(gutter) + statusWidth
)

// essentialWidth is what a line costs beside its name: the space after the
// name, the two mpb writes around the bar, the bar, and the columns that are
// drawn however narrow the terminal is.
//
// It is what the name column is measured against, because the name is the one
// column with no width of its own to hold it back. mpb draws the decorators
// first and gives the bar what they left, so a name column that takes the
// terminal takes it from the bar -- and a bar drawn to whatever was left is a
// bar of a different width on every row, since what was left depends on how
// long that row's status happened to be.
const (
	essentialWidth = 3 + barWidth + percentColumn + statusColumn
	// minNameWidth is what the name column keeps on a terminal too narrow for
	// even that. Below this the line is over budget whatever DAC does, and mpb
	// cuts it at the edge; the names are what it should still be cutting from
	// the least.
	minNameWidth = 8
)

type bars struct {
	mutex   sync.Mutex
	output  *mpb.Progress
	entries map[string]*barState
	palette style.Palette
	// name is the width of the name column, read by every bar on every frame
	// and only ever widened. A column that can narrow is a display that jumps
	// when an asset finishes, which is the moment there is least to be gained
	// by moving anything.
	name atomic.Int64
}

type barState struct {
	mutex  sync.Mutex
	bar    *mpb.Bar
	total  int64
	status string
	// settled records that this transfer reached an outcome. Only a bar that
	// never did is taken off the display by Wait, and the two are not the same
	// question as whether mpb still considers the bar running: an aborted bar has
	// settled, and a bar the command has stopped touching has not.
	settled bool
	// look colours the status word, and is nil while there is nothing to say
	// about it. A bar downloads until something settles it, and what settles it
	// is either a completion or a failure -- so the colour comes from which of
	// those two happened rather than from reading the word they left behind,
	// and a status added later is coloured correctly without being listed here.
	look func(string) string
	// start and elapsed bound the window the speed column reports over. elapsed
	// is zero while the transfer is running and is fixed when it settles,
	// because a finished row is drawn a few more times before it leaves the
	// screen and a rate that kept dividing by a growing clock would decay on the
	// way out.
	start   time.Time
	elapsed time.Duration
	// moved counts the bytes this transfer actually carried, which is not what
	// the bar's own current count means once the asset settles: completing a bar
	// fills it to its total, so an asset answered from the cache would otherwise
	// report having moved every byte of itself in no time at all.
	moved atomic.Int64
}

// newBars builds the bar container on the command's context.
//
// Wait blocks until every bar it started has finished, and a transfer that an
// interrupt cuts short finishes none of them: the command reports nothing for
// an asset whose context was cancelled, because that message would describe the
// interrupt rather than the asset. Building the container on the same context
// makes the cancellation itself end those bars, so Wait returns and the process
// exits. Without it Ctrl+C stopped every download and then hung in Wait, with
// no signal left that could get the operator out of it.
func newBars(ctx context.Context, writer io.Writer, palette style.Palette) *bars {
	return &bars{
		output:  mpb.NewWithContext(ctx, mpb.WithOutput(writer), mpb.ForceAutoRefresh(), mpb.PopCompletedMode()),
		entries: map[string]*barState{},
		palette: palette,
	}
}

// Plan sizes the name column for the assets a command is about to work through.
//
// Every other column has a width DAC can state in advance; this one is as wide
// as the longest name it will hold, and a name is not known until an asset
// starts. Starting them is what a command spends its time doing, so a column
// measured from the bars on screen would widen every time a longer name turned
// up and shift every row that was already drawn -- and the rows PopCompletedMode
// has already printed above the display cannot be shifted at all. Being told
// the set first settles the width before the first bar is drawn.
//
// It is the set a command will report on rather than the set it will report:
// which assets a lock has to reach an origin for is not known until it asks,
// and a column sized for a name that turns out to cost nothing is padding,
// whereas one sized without a name that turns up later is a display that moves.
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
	// The status is padded even though nothing is drawn after it, because what
	// is drawn *before* it depends on how wide it is: mpb fills the bar with
	// what the decorators left over, so an unpadded status would give a row
	// saying "cached" a wider bar than the row above it still downloading.
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
		// mpb writes one space between the bar and this group, and the columns
		// carry the other one each.
		decor.Any(func(stat decor.Statistics) string {
			return " " + percentage(stat.Current, total)
		}),
		// The two columns that give way to a narrow terminal are drawn together,
		// because whether the second one is drawn depends on whether the first
		// one was and a decorator is not told what the decorator before it did.
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
		// The gutter in front of the status is drawn on its own so that what the
		// palette colours is the word and not the space before it.
		decor.Any(func(decor.Statistics) string { return gutter }),
		// The status is last because it is the one column whose longest value
		// DAC does not decide: the words come from the service, and a new one
		// that outgrows the column here runs off the end of the line rather than
		// pushing another column sideways.
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
// It takes down any bar the command left running, because mpb waits for every
// bar it was given and a command does not settle every bar it starts. The first
// asset to fail cancels the rest, and an asset cut short that way is reported
// as nothing at all: the only message it could carry would describe another
// asset's failure. That is the right thing to say and the wrong thing to leave
// on screen -- the transfer is over either way, and a bar nobody will ever
// finish held the whole process open behind a display that had stopped moving.
// A pull whose origin does not resolve is the ordinary way to reach that, since
// one unreachable URL cancels every transfer beside it.
//
// The bars come off rather than staying: a row still reading "downloading" for
// a transfer that has stopped is a worse answer than no row, and the failure
// that ended the run is printed after this returns. The line form ends those
// assets with no line either, which is the same silence for the same reason.
//
// Every operation calls this once its transfers have finished, so a bar still
// unsettled here is one nothing was ever going to settle.
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

// nameWidth is how wide the name column is drawn on this frame: as wide as the
// longest name the command reports on, and never wider than the terminal can
// hold beside the columns that have to be there.
//
// It is read from the first decorator on the line, where mpb has not yet taken
// anything off the width it reports, so what it sees is the terminal itself.
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
//
// It is mpb's answer to a string that got longer without getting wider: the
// decorator reports its own width, and the escape sequences go on afterwards.
// Colouring inside the decorator instead would count them as characters, and
// every column on the line would drift by however many bytes the terminal never
// draws.
//
// A reporter that colours nothing hands the decorator back untouched rather
// than routing every redraw of every bar through a function that returns its
// argument.
func (reporter *bars) meta(decorator decor.Decorator, look func(string) string) decor.Decorator {
	if !reporter.palette.Enabled() {
		return decorator
	}
	return decor.Meta(decorator, look)
}

// barStyle colours the bar itself: the part that is done takes the accent the
// asset names take, and the part still to go recedes. A screen of bars then
// reads as how far along it is rather than as a wall of brackets. A palette
// that colours nothing leaves every meta nil, which is what mpb reads as "write
// these bytes".
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

// fits reports whether a column of this width still leaves the line room for
// the bar and for the status behind it.
//
// mpb draws the decorators first and gives the bar whatever they left, then
// cuts the line at the terminal's edge if they left nothing. So a full set of
// columns on a narrow terminal costs the bar first and the status second, which
// is the display losing the two things it is for and keeping a byte count. A
// column that will not fit is not drawn at all instead: the ones that stay are
// in the same place they were, and a terminal wide enough is unaffected.
//
// The width mpb reports is what is left of the terminal after the columns
// already drawn, and every bar spends the same widths in the same order, so
// every row of one frame reaches the same answer. It changes when the terminal
// is resized, which is the one time a display is meant to be laid out again.
func fits(stat decor.Statistics, column int) bool {
	// The two spaces mpb writes around the bar are subtracted after the
	// decorators have run, so they are still owed here.
	return stat.AvailableWidth-barWidth-2-statusColumn >= column
}

// counters draws how much of an asset has arrived against how much of it there
// is, as two amounts in fixed columns.
//
// An asset whose size the origin never declared has no second figure, and the
// first one is drawn where the second would be rather than where a first
// normally goes: what lines up down the screen then is the number that is
// actually being compared with its neighbours, and the gap left in front of it
// says there is nothing to compare it against.
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
//
// A transfer that has moved nothing has no speed rather than a speed of zero,
// and the difference matters here: an asset answered from the cache moved no
// bytes at all, and "0 B/s" reads as a stalled download rather than as one that
// never happened.
func rate(moved int64, window time.Duration) string {
	if moved <= 0 || window <= 0 {
		return blank(rateWidth)
	}
	number, unit, _ := strings.Cut(bytesize.Format(int64(float64(moved)/window.Seconds())), " ")
	return pad(number, sizeWidth, true) + " " + pad(unit+"/s", rateUnitWidth, false)
}

// percentage draws how far along a transfer is, and nothing at all for one whose
// total nobody declared. A bar with no total has no percentage to report, and a
// nought there would be a measurement rather than a gap.
//
// An asset with no bytes in it is all of the way through rather than none of
// it, which is the reading that agrees with the bar beside it: mpb completes a
// bar whose current has reached its total, and nought has.
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

// pad returns text drawn into width, against the right edge of the column or
// the left. Text that is already wider is returned as it is: a number cut to
// fit is a different number, and the columns that could overflow are the ones
// with nothing to their right.
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

// clip shortens a name too long for its column, marking the cut so that nobody
// reads what is left as the whole coordinate.
func clip(text string, width int) string {
	if utf8.RuneCountInString(text) <= width || width < 1 {
		return text
	}
	return string([]rune(text)[:width-1]) + "…"
}

// blank is a column with nothing in it.
func blank(width int) string { return strings.Repeat(" ", width) }
