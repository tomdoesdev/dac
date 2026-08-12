package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/tomdoesdev/dac/cmd/dac/command"
	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/output"
	"github.com/tomdoesdev/kit/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// BuildVersion is replaced by release builds. Development builds deliberately
// retain an obvious value instead of pretending to be a published version.
var BuildVersion = "dev"

// Run creates and dispatches one dac invocation. It returns the conventional
// CLI exit status while allowing tests to provide isolated streams.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	options := &output.Options{}
	writer := output.New(options, stdout, stderr)
	downloader := asset.NewDownloader(asset.NewHTTPClient(), "dac/"+BuildVersion)

	app := cli.New(ctx, cli.WithName("dac"), cli.WithVersion(BuildVersion), cli.WithOutput(stdout), cli.WithErrorOutput(stderr))
	app.MustAddGlobalFlags(options)
	command.Register(app, command.Dependencies{Output: writer, Downloader: downloader, CWD: os.Getwd})
	if err := app.Run(args); err != nil {
		writer.Error(err)
		return exitCode(err)
	}
	return 0
}

// exitCode maps a classified failure onto the conventional CLI status. Telling
// a caller apart from an operator is a process-boundary decision, so it lives
// here rather than in the code that renders the message.
func exitCode(err error) int {
	var usage *cli.UsageError
	if errors.As(err, &usage) {
		return 2
	}
	var operation *fault.Error
	if errors.As(err, &operation) && operation.Kind() == fault.ErrorKindConfiguration {
		return 2
	}
	return 1
}
