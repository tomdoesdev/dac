// Package dac wires the command-line surface to project state transitions.
package dac

import (
	"context"
	"io"
	"os"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/command"
	"github.com/tomdoesdev/dac/internal/output"
	"github.com/tomdoesdev/kit/cli"
)

// BuildVersion is replaced by release builds. Development builds deliberately
// retain an obvious value instead of pretending to be a published version.
var BuildVersion = "dev"

// Run creates and dispatches one dac invocation. It returns the conventional
// CLI exit status while allowing tests to provide isolated streams.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	options := &output.Options{}
	writer := output.New(options, stdout)
	downloader := asset.NewDownloader(asset.NewHTTPClient(), "dac/"+BuildVersion)

	app := cli.New(ctx, cli.WithName("dac"), cli.WithVersion(BuildVersion), cli.WithOutput(stdout), cli.WithErrorOutput(stderr))
	app.MustAddGlobalFlags(options)
	command.Register(app, command.Dependencies{Output: writer, Downloader: downloader, CWD: os.Getwd})
	if err := app.Run(args); err != nil {
		return writer.Error(stderr, err)
	}
	return 0
}
