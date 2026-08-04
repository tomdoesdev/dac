// Package progress writes asset transfer progress to standard error.
package progress

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"

	"github.com/tomdoesdev/dac/internal/application"
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

type bars struct {
	mutex   sync.Mutex
	output  *mpb.Progress
	entries map[string]*barState
	palette style.Palette
}

type barState struct {
	mutex  sync.Mutex
	bar    *mpb.Bar
	total  int64
	status string
	// look colours the status word, and is nil while there is nothing to say
	// about it. A bar downloads until something settles it, and what settles it
	// is either a completion or a failure -- so the colour comes from which of
	// those two happened rather than from reading the word they left behind,
	// and a status added later is coloured correctly without being listed here.
	look func(string) string
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

func (reporter *bars) Start(name string, total int64) {
	reporter.mutex.Lock()
	defer reporter.mutex.Unlock()
	if _, exists := reporter.entries[name]; exists {
		return
	}
	state := &barState{total: total, status: "downloading"}
	status := decor.Any(func(decor.Statistics) string {
		state.mutex.Lock()
		defer state.mutex.Unlock()
		return state.status
	}, decor.WCSyncSpace)
	status = reporter.meta(status, func(text string) string {
		state.mutex.Lock()
		defer state.mutex.Unlock()
		if state.look == nil {
			return text
		}
		return state.look(text)
	})
	appenders := []decor.Decorator{
		decor.CountersKibiByte("% .1f / % .1f", decor.WCSyncSpace),
		decor.AverageSpeed(decor.SizeB1024(0), "% .1f", decor.WCSyncSpace),
		status,
	}
	if total >= 0 {
		appenders = append(appenders, decor.Percentage(decor.WCSyncSpace))
	}
	options := []mpb.BarOption{
		mpb.BarWidth(24),
		mpb.PrependDecorators(reporter.meta(decor.Name(name+" ", decor.WCSyncSpace), reporter.palette.Name)),
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
		state.bar.IncrInt64(count)
	}
}

func (reporter *bars) Done(name, status string) {
	state := reporter.entry(name)
	if state == nil {
		_, _ = fmt.Fprintf(reporter.output, "done %s %s\n", name, status)
		return
	}
	state.mutex.Lock()
	state.status, state.look = status, reporter.palette.Good
	state.mutex.Unlock()
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
	state.mutex.Lock()
	state.status, state.look = "failed", reporter.palette.Bad
	state.mutex.Unlock()
	state.bar.Abort(false)
}

func (reporter *bars) Wait() { reporter.output.Wait() }

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
