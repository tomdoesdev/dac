package cli

import (
	"context"
	"fmt"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
)

// checkCommand builds the read-only project and upstream consistency command.
func (runner *runner) checkCommand() *urfave.Command {
	flags := append(runner.networkFlags(true),
		&urfave.BoolFlag{Name: "upstream", Usage: "Check that each origin still serves the locked bytes."},
	)
	return &urfave.Command{
		Name:  "check",
		Usage: "Check that the manifest, lock, and optional upstream bytes agree.",
		Flags: flags,
		Action: runner.runBare("check", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			// The plain form deliberately constructs no network, cache, trust, or
			// configuration dependencies. Its entire scope is the two project files.
			if !current.Bool("upstream") {
				manifest, lock := projectPaths(current)
				result, err := application.New(manifest, lock, nil, nil, nil).Check(ctx, application.CheckOptions{})
				return result, runner.stdoutPalette.Good("The manifest and lock file agree."), err
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
			result, err := service.Check(ctx, application.CheckOptions{
				Concurrency: concurrency,
				MaxSize:     maxSize,
				Upstream:    true,
			})
			message := fmt.Sprintf("The upstream bytes match the lock for %s. Downloaded %s for validation.",
				runner.stdoutPalette.Strong(plural(result.AssetCount, "asset")),
				runner.stdoutPalette.Strong(plural(result.Downloaded, "asset")))
			return result, message, err
		}),
	}
}
