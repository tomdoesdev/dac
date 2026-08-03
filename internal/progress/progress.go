// Package progress writes asset transfer progress to standard error.
package progress

import (
	"fmt"
	"io"
	"sync"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"

	"github.com/tom/dac/internal/application"
)

// New selects bars for a terminal and lines for other writers.
func New(writer io.Writer, terminal, enabled bool) application.Reporter {
	if !enabled {
		return application.NopReporter{}
	}
	if terminal {
		return newBars(writer)
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
}

type barState struct {
	mutex  sync.Mutex
	bar    *mpb.Bar
	total  int64
	status string
}

func newBars(writer io.Writer) *bars {
	return &bars{
		output:  mpb.New(mpb.WithOutput(writer), mpb.ForceAutoRefresh(), mpb.PopCompletedMode()),
		entries: map[string]*barState{},
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
		mpb.PrependDecorators(decor.Name(name+" ", decor.WCSyncSpace)),
		mpb.AppendDecorators(appenders...),
	}
	if total >= 0 {
		state.bar = reporter.output.New(0, mpb.BarStyle(), options...)
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
	state.status = status
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
	state.status = "failed"
	state.mutex.Unlock()
	state.bar.Abort(false)
}

func (reporter *bars) Wait() { reporter.output.Wait() }

func (reporter *bars) entry(name string) *barState {
	reporter.mutex.Lock()
	defer reporter.mutex.Unlock()
	return reporter.entries[name]
}
