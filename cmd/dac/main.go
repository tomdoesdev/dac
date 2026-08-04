// Command dac manages reproducible assets in a content-addressed cache.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/tomdoesdev/dac/internal/cli"
)

func main() {
	// Ignore SIGPIPE so the writer can stop without a second error document.
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := interrupted()
	status := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(status)
}

// interrupted returns a context cancelled by the first interrupt, and hands the
// next one back to the operating system as it cancels.
//
// The handing back is the point. signal.NotifyContext leaves its handler
// installed for the life of the process, so every interrupt after the first is
// delivered to DAC and dropped: an operator watching a command that has not
// stopped presses Ctrl+C again and nothing happens, because the only thing
// listening has already cancelled and moved on. Resetting restores the default
// action, so the second press terminates the process however far along the
// shutdown is. A clean cancellation still wins the race in every ordinary case
// -- this is the way out when it does not.
//
// What a terminated shutdown costs is a partial download left in the cache's
// temporary directory, which is already what an interrupted transfer leaves and
// already what dac cache gc sweeps. No object is installed except by a rename,
// so nothing a later command reads can be half written.
func interrupted() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer signal.Stop(signals)
		select {
		case <-signals:
			signal.Reset(os.Interrupt, syscall.SIGTERM)
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
