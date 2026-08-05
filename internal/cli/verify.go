package cli

import (
	"context"
	"fmt"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
)

func (runner *runner) verifyCommand() *urfave.Command {
	flags := append(runner.networkFlags(true),
		&urfave.BoolFlag{Name: "refresh", Usage: "Resolve every asset against its origin and report drift."},
	)
	return &urfave.Command{
		Name:  "verify",
		Usage: "Check that the project files agree.",
		Flags: flags,
		Action: runner.run("verify", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
				return nil, "", err
			}
			// The plain form reads two files and stops, which is what makes it
			// usable in a job with no network and no cache. Only a refresh needs
			// either, so only a refresh builds a service that has them.
			if !current.Bool("refresh") {
				result, err := runner.projectService(current).Verify(ctx, application.VerifyOptions{})
				// The whole sentence is the finding here, rather than a fact
				// with a state buried in it, so the whole sentence carries it.
				return result, runner.stdoutPalette.Good("Project files are valid."), err
			}
			service, client, err := runner.networkService(ctx, current, false)
			if err != nil {
				return nil, "", err
			}
			defer client.Close()
			concurrency, err := runner.concurrency(current)
			if err != nil {
				return nil, "", err
			}
			maxSize, err := runner.maximumSize(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.Verify(ctx, application.VerifyOptions{
				Concurrency: concurrency,
				MaxSize:     maxSize,
				Refresh:     true,
			})
			return result, fmt.Sprintf("The origins still serve the locked bytes for %s.",
				runner.stdoutPalette.Strong(plural(result.AssetCount, "asset"))), err
		}),
	}
}
