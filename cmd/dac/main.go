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

// interrupted returns a context cancelled by the first interrupt, and hands the next one back to the operating system as it cancels.
// The handing back is the point.
// A forced shutdown can leave a partial download that cache collection removes.
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
