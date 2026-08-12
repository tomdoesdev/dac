package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestPingPongCursor pins the endpoint behavior that distinguishes a throbber
// from a looping spinner: each end appears once before direction reverses.
func TestPingPongCursor(t *testing.T) {
	cursor := pingPongCursor{length: 6, direction: 1}
	indices := []int{cursor.index}
	for range 11 {
		cursor.advance()
		indices = append(indices, cursor.index)
	}
	want := []int{0, 1, 2, 3, 4, 5, 4, 3, 2, 1, 0, 1}
	if len(indices) != len(want) {
		t.Fatalf("indices = %v, want %v", indices, want)
	}
	for index := range want {
		if indices[index] != want[index] {
			t.Fatalf("indices = %v, want %v", indices, want)
		}
	}

	single := pingPongCursor{length: 1, direction: 1}
	single.advance()
	if single.index != 0 {
		t.Fatalf("single glyph index = %d, want 0", single.index)
	}
}

// TestThrobberRunRendersAndCompletes proves that the permanent line is emitted
// only after the animation line has been cleared and the worker has stopped.
func TestThrobberRunRendersAndCompletes(t *testing.T) {
	output := &bytes.Buffer{}
	var end ThrobberEnd
	throbber := NewThrobber(
		output,
		"Working…",
		WithThrobberMode(ThrobberAlways),
		WithThrobberInterval(time.Hour),
		WithThrobberGlyphs("*"),
		WithThrobberStyle(func(glyph string) string { return "[" + glyph + "]" }),
		WithThrobberOnEnd(func(value ThrobberEnd) string {
			end = value
			return "done"
		}),
	)
	if err := throbber.Run(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "\r\x1b[2K[*] Working… (0s)\r\x1b[2Kdone\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if end.Glyph != "*" || !end.Animated || end.Err != nil || end.Elapsed < 0 {
		t.Fatalf("end = %#v", end)
	}
}

// TestThrobberInactiveModes still invokes lifecycle hooks while leaving
// redirected output untouched unless the hook explicitly returns a line.
func TestThrobberInactiveModes(t *testing.T) {
	for name, mode := range map[string]ThrobberMode{"auto": ThrobberAuto, "never": ThrobberNever} {
		t.Run(name, func(t *testing.T) {
			output := &bytes.Buffer{}
			calls := 0
			throbber := NewThrobber(output, "Working…", WithThrobberMode(mode), WithThrobberOnEnd(func(end ThrobberEnd) string {
				calls++
				if end.Animated {
					t.Fatalf("inactive end = %#v", end)
				}
				return ""
			}))
			if err := throbber.Run(context.Background(), func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || output.Len() != 0 {
				t.Fatalf("calls=%d output=%q", calls, output.String())
			}
		})
	}
}

// TestAnimatesAnswersForCallersSharingTheLine covers the question a caller has
// to ask before writing a permanent line beside a running throbber: whether
// there is a frame on screen to erase at all. Its answer must be the one the
// throbber itself acts on.
func TestAnimatesAnswersForCallersSharingTheLine(t *testing.T) {
	output := &bytes.Buffer{}
	for mode, want := range map[ThrobberMode]bool{ThrobberAlways: true, ThrobberNever: false, ThrobberAuto: false} {
		throbber := NewThrobber(output, "Working…", WithThrobberMode(mode))
		if got := Animates(output, mode); got != want || got != throbber.animates() {
			t.Fatalf("Animates(mode %d) = %v, want %v and the throbber's own answer", mode, got, want)
		}
	}
	defer func() {
		if recover() == nil {
			t.Fatal("an invalid mode was accepted")
		}
	}()
	Animates(output, ThrobberMode(200))
}

// TestThrobberFailuresClearWithoutLosingThePrimaryError covers both operation
// and rendering failures, which may occur together after work has begun.
func TestThrobberFailuresClearWithoutLosingThePrimaryError(t *testing.T) {
	operationErr := errors.New("operation failed")
	t.Run("operation", func(t *testing.T) {
		output := &bytes.Buffer{}
		calls := 0
		throbber := NewThrobber(output, "Working…", WithThrobberMode(ThrobberAlways), WithThrobberInterval(time.Hour), WithThrobberOnEnd(func(end ThrobberEnd) string {
			calls++
			if !errors.Is(end.Err, operationErr) {
				t.Fatalf("end error = %v", end.Err)
			}
			return ""
		}))
		err := throbber.Run(context.Background(), func(context.Context) error { return operationErr })
		if !errors.Is(err, operationErr) || calls != 1 || !strings.HasSuffix(output.String(), "\r\x1b[2K") {
			t.Fatalf("error=%v calls=%d output=%q", err, calls, output.String())
		}
	})

	t.Run("rendering", func(t *testing.T) {
		writeErr := errors.New("write failed")
		output := &failAfterWriter{remaining: 1, err: writeErr}
		throbber := NewThrobber(output, "Working…", WithThrobberMode(ThrobberAlways), WithThrobberInterval(time.Hour))
		err := throbber.Run(context.Background(), func(context.Context) error { return operationErr })
		if !errors.Is(err, operationErr) || !errors.Is(err, writeErr) {
			t.Fatalf("joined error = %v", err)
		}
	})
}

// TestThrobberCancellationAndPanicStopTheWorker protects terminal cleanup on
// the two early-exit paths most likely to otherwise leave a transient line.
func TestThrobberCancellationAndPanicStopTheWorker(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		output := &bytes.Buffer{}
		throbber := NewThrobber(output, "Working…", WithThrobberMode(ThrobberAlways), WithThrobberInterval(time.Hour))
		err := throbber.Run(ctx, func(ctx context.Context) error {
			cancel()
			<-ctx.Done()
			return ctx.Err()
		})
		if !errors.Is(err, context.Canceled) || !strings.HasSuffix(output.String(), "\r\x1b[2K") {
			t.Fatalf("error=%v output=%q", err, output.String())
		}
	})

	t.Run("panic", func(t *testing.T) {
		output := &bytes.Buffer{}
		calls := 0
		panicValue := errors.New("boom")
		throbber := NewThrobber(output, "Working…", WithThrobberMode(ThrobberAlways), WithThrobberInterval(time.Hour), WithThrobberOnEnd(func(end ThrobberEnd) string {
			calls++
			if end.Err == nil {
				t.Fatal("panic end hook received no error")
			}
			return ""
		}))
		func() {
			defer func() {
				if recovered := recover(); recovered != panicValue {
					t.Fatalf("recovered = %#v, want %#v", recovered, panicValue)
				}
			}()
			_ = throbber.Run(context.Background(), func(context.Context) error { panic(panicValue) })
		}()
		if calls != 1 || !strings.HasSuffix(output.String(), "\r\x1b[2K") {
			t.Fatalf("calls=%d output=%q", calls, output.String())
		}
	})
}

func TestFormatThrobberElapsedUsesWholeSeconds(t *testing.T) {
	for input, want := range map[time.Duration]string{-time.Second: "0s", 500 * time.Millisecond: "0s", 1500 * time.Millisecond: "1s", 65 * time.Second: "1m5s"} {
		if got := formatThrobberElapsed(input); got != want {
			t.Fatalf("formatThrobberElapsed(%s) = %q, want %q", input, got, want)
		}
	}
}

// TestThrobberRejectsInvalidConfiguration keeps malformed single-line output
// and unusable lifecycle options from failing later in a worker goroutine.
func TestThrobberRejectsInvalidConfiguration(t *testing.T) {
	output := &bytes.Buffer{}
	constructors := map[string]func(){
		"nil output":        func() { NewThrobber(nil, "Working…") },
		"empty message":     func() { NewThrobber(output, " ") },
		"multiline message": func() { NewThrobber(output, "one\ntwo") },
		"missing glyphs":    func() { NewThrobber(output, "Working…", WithThrobberGlyphs()) },
		"empty glyph":       func() { NewThrobber(output, "Working…", WithThrobberGlyphs("")) },
		"zero interval":     func() { NewThrobber(output, "Working…", WithThrobberInterval(0)) },
		"invalid mode":      func() { NewThrobber(output, "Working…", WithThrobberMode(ThrobberMode(255))) },
		"nil style":         func() { NewThrobber(output, "Working…", WithThrobberStyle(nil)) },
		"nil end hook":      func() { NewThrobber(output, "Working…", WithThrobberOnEnd(nil)) },
		"nil option":        func() { NewThrobber(output, "Working…", nil) },
	}
	for name, construct := range constructors {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("invalid throbber configuration did not panic")
				}
			}()
			construct()
		})
	}
}

type failAfterWriter struct {
	remaining int
	err       error
}

// Write fails after a configured number of successful writes so tests can
// exercise errors that occur during cleanup rather than initial rendering.
func (writer *failAfterWriter) Write(value []byte) (int, error) {
	if writer.remaining == 0 {
		return 0, writer.err
	}
	writer.remaining--
	return len(value), nil
}
