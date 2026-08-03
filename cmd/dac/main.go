// Command dac manages reproducible assets in a content-addressed cache.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/tom/dac/internal/cli"
)

func main() {
	// Ignore SIGPIPE so the writer can stop without a second error document.
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	status := cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(status)
}
