package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
)

const defaultThrobberInterval = 100 * time.Millisecond

var (
	defaultThrobberGlyphs = []string{"·", "✢", "✳", "∗", "✻", "✽"}
	errThrobberPanicked   = errors.New("throbber operation panicked")
)

// ThrobberMode controls whether a throbber emits terminal animation.
type ThrobberMode uint8

const (
	// ThrobberAuto animates only when its output is an interactive terminal.
	ThrobberAuto ThrobberMode = iota
	// ThrobberAlways animates regardless of output and is useful in tests.
	ThrobberAlways
	// ThrobberNever runs the operation without emitting animation.
	ThrobberNever
)

// ThrobberEnd describes an operation after its animation worker has stopped.
// Err is non-nil when the operation or terminal rendering failed.
type ThrobberEnd struct {
	Glyph    string
	Elapsed  time.Duration
	Animated bool
	Err      error
}

// ThrobberOption customizes a throbber without exposing its rendering internals.
type ThrobberOption func(*throbberConfig)

type throbberConfig struct {
	glyphs   []string
	interval time.Duration
	mode     ThrobberMode
	style    func(string) string
	onEnd    func(ThrobberEnd) string
}

// WithThrobberGlyphs replaces the ordered glyph set that the animation walks
// forwards and backwards without repeating either endpoint.
func WithThrobberGlyphs(glyphs ...string) ThrobberOption {
	if len(glyphs) == 0 {
		panic("cli: throbber glyphs must not be empty")
	}
	values := append([]string(nil), glyphs...)
	for _, glyph := range values {
		if glyph == "" || strings.ContainsAny(glyph, "\r\n") {
			panic("cli: throbber glyphs must be non-empty single-line strings")
		}
	}
	return func(config *throbberConfig) { config.glyphs = values }
}

// WithThrobberInterval sets the delay between animation frames.
func WithThrobberInterval(interval time.Duration) ThrobberOption {
	if interval <= 0 {
		panic("cli: throbber interval must be positive")
	}
	return func(config *throbberConfig) { config.interval = interval }
}

// WithThrobberMode selects automatic, forced, or disabled animation.
func WithThrobberMode(mode ThrobberMode) ThrobberOption {
	validateThrobberMode(mode)
	return func(config *throbberConfig) { config.mode = mode }
}

// WithThrobberStyle formats each glyph. A Styler method value can provide
// terminal-aware color without coupling callers to an ANSI implementation.
func WithThrobberStyle(style func(string) string) ThrobberOption {
	if style == nil {
		panic("cli: throbber style must not be nil")
	}
	return func(config *throbberConfig) { config.style = style }
}

// WithThrobberOnEnd sets a callback that runs exactly once after animation has
// stopped. A non-empty return value is written as a permanent, newline-terminated line.
func WithThrobberOnEnd(onEnd func(ThrobberEnd) string) ThrobberOption {
	if onEnd == nil {
		panic("cli: throbber end callback must not be nil")
	}
	return func(config *throbberConfig) { config.onEnd = onEnd }
}

// Throbber renders one transient status line while a scoped operation runs.
// Run calls are serialized so a reusable throbber cannot corrupt its own line.
type Throbber struct {
	output  io.Writer
	message string
	config  throbberConfig
	runMu   sync.Mutex
}

// NewThrobber creates a ping-pong terminal animation. The message must already
// contain any punctuation, such as an ellipsis, that should precede elapsed time.
func NewThrobber(output io.Writer, message string, options ...ThrobberOption) *Throbber {
	if output == nil {
		panic("cli: throbber output writer must not be nil")
	}
	if strings.TrimSpace(message) == "" || strings.ContainsAny(message, "\r\n") {
		panic("cli: throbber message must be a non-empty single-line string")
	}
	styler := NewStyler(output, ColorAuto)
	config := throbberConfig{
		glyphs:   append([]string(nil), defaultThrobberGlyphs...),
		interval: defaultThrobberInterval,
		mode:     ThrobberAuto,
		style:    styler.Progress,
	}
	for _, option := range options {
		if option == nil {
			panic("cli: throbber option must not be nil")
		}
		option(&config)
	}
	return &Throbber{output: output, message: message, config: config}
}

// Run animates while action executes in the caller's goroutine. It always
// stops and clears the worker before invoking the end callback or returning.
func (throbber *Throbber) Run(ctx context.Context, action func(context.Context) error) (runErr error) {
	if ctx == nil {
		panic("cli: throbber context must not be nil")
	}
	if action == nil {
		panic("cli: throbber action must not be nil")
	}

	throbber.runMu.Lock()
	defer throbber.runMu.Unlock()

	started := time.Now()
	animation := throbber.startAnimation(ctx, started)
	defer func() {
		if panicValue := recover(); panicValue != nil {
			// Preserve the operation's original panic after best-effort cleanup and
			// notification. An end-hook panic must not replace the primary failure.
			func() {
				defer func() { _ = recover() }()
				_ = throbber.finish(animation, started, errThrobberPanicked)
			}()
			panic(panicValue)
		}
		runErr = errors.Join(runErr, throbber.finish(animation, started, runErr))
	}()

	runErr = action(ctx)
	return runErr
}

// validateThrobberMode keeps invalid public configuration failures immediate.
func validateThrobberMode(mode ThrobberMode) {
	switch mode {
	case ThrobberAuto, ThrobberAlways, ThrobberNever:
		return
	default:
		panic("cli: invalid throbber mode")
	}
}

// throbberAnimation owns the single rendering goroutine used by one Run call.
type throbberAnimation struct {
	throbber *Throbber
	animated bool
	rendered bool
	cursor   pingPongCursor
	stop     chan struct{}
	done     chan struct{}
	err      error
}

// startAnimation performs the first write synchronously so immediate output
// failures are recorded before the operation begins and no failed worker starts.
func (throbber *Throbber) startAnimation(ctx context.Context, started time.Time) *throbberAnimation {
	animation := &throbberAnimation{
		throbber: throbber,
		animated: throbber.animates(),
		cursor:   pingPongCursor{length: len(throbber.config.glyphs), direction: 1},
	}
	if !animation.animated {
		return animation
	}
	if animation.err = throbber.render(animation.cursor.index, time.Since(started)); animation.err != nil {
		return animation
	}
	animation.rendered = true
	animation.stop = make(chan struct{})
	animation.done = make(chan struct{})
	go animation.loop(ctx, started)
	return animation
}

// loop is the only goroutine that advances frames, so stopping it before the
// final clear makes the output ordering deterministic without a write mutex.
func (animation *throbberAnimation) loop(ctx context.Context, started time.Time) {
	defer close(animation.done)
	ticker := time.NewTicker(animation.throbber.config.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-animation.stop:
			return
		case <-ticker.C:
			animation.cursor.advance()
			if err := animation.throbber.render(animation.cursor.index, time.Since(started)); err != nil {
				animation.err = err
				return
			}
		}
	}
}

// finish joins the worker, clears its transient line, then calls onEnd. This
// ordering lets hooks safely leave a permanent replacement on the same writer.
func (throbber *Throbber) finish(animation *throbberAnimation, started time.Time, operationErr error) error {
	if animation.done != nil {
		close(animation.stop)
		<-animation.done
	}
	renderErr := animation.err
	if animation.rendered {
		_, clearErr := io.WriteString(throbber.output, "\r\x1b[2K")
		renderErr = errors.Join(renderErr, clearErr)
	}

	endErr := errors.Join(operationErr, renderErr)
	if throbber.config.onEnd == nil {
		return renderErr
	}
	message := throbber.config.onEnd(ThrobberEnd{
		Glyph:    throbber.config.glyphs[animation.cursor.index],
		Elapsed:  time.Since(started),
		Animated: animation.animated && animation.rendered,
		Err:      endErr,
	})
	if message == "" {
		return renderErr
	}
	if strings.ContainsAny(message, "\r\n") {
		return errors.Join(renderErr, errors.New("cli: throbber end message must be a single line"))
	}
	_, writeErr := fmt.Fprintln(throbber.output, message)
	return errors.Join(renderErr, writeErr)
}

// animates separates motion policy from color policy: forcing ANSI color does
// not make a redirected stream safe for cursor-control sequences.
func (throbber *Throbber) animates() bool {
	return Animates(throbber.output, throbber.config.mode)
}

// Animates reports whether a throbber created for output with mode would emit
// animation. A caller that shares the line a throbber is using, such as one
// leaving a permanent record beside a frame that is still on screen, has to
// erase that frame first, and needs this same answer before it writes any
// cursor-control sequence of its own.
func Animates(output io.Writer, mode ThrobberMode) bool {
	validateThrobberMode(mode)
	switch mode {
	case ThrobberAlways:
		return true
	case ThrobberNever:
		return false
	default:
		return terminalWriter(output) && os.Getenv("TERM") != "dumb"
	}
}

// render rewrites the entire terminal line so elapsed text can grow without
// leaving bytes from the previous frame behind.
func (throbber *Throbber) render(index int, elapsed time.Duration) error {
	styledGlyph := throbber.config.style(throbber.config.glyphs[index])
	if strings.ContainsAny(styledGlyph, "\r\n") {
		return errors.New("cli: styled throbber glyph must be a single line")
	}
	_, err := fmt.Fprintf(throbber.output, "\r\x1b[2K%s %s (%s)", styledGlyph, throbber.message, formatThrobberElapsed(elapsed))
	return err
}

// terminalWriter detects real and Cygwin terminals without treating forced
// color on a pipe as permission to emit cursor-control sequences.
func terminalWriter(output io.Writer) bool {
	file, ok := output.(interface{ Fd() uintptr })
	return ok && (isatty.IsTerminal(file.Fd()) || isatty.IsCygwinTerminal(file.Fd()))
}

// formatThrobberElapsed removes sub-second noise while retaining duration's
// compact minute and hour formatting for longer operations.
func formatThrobberElapsed(elapsed time.Duration) string {
	if elapsed < 0 {
		elapsed = 0
	}
	return elapsed.Truncate(time.Second).String()
}

// pingPongCursor walks an ordered glyph set without duplicating the endpoints
// when it reverses direction.
type pingPongCursor struct {
	length    int
	index     int
	direction int
}

func (cursor *pingPongCursor) advance() {
	if cursor.length < 2 {
		return
	}
	next := cursor.index + cursor.direction
	if next >= cursor.length {
		cursor.direction = -1
		next = cursor.length - 2
	} else if next < 0 {
		cursor.direction = 1
		next = 1
	}
	cursor.index = next
}
