// Command dac locks and reproducibly downloads arbitrary HTTP(S) artifacts.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/tomdoesdev/dac/internal/dac"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(dac.Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
