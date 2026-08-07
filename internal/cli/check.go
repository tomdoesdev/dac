package cli

import (
	"context"
	"fmt"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/coord"
)

// checkCommand builds the offline project check and its explicit upstream mode.
func (runner *runner) checkCommand() *urfave.Command {
	return &urfave.Command{
		Name:            "check",
		Usage:           "Check that the manifest and lock file agree.",
		HideHelpCommand: true,
		Commands:        []*urfave.Command{runner.checkUpstreamCommand()},
		Action: runner.runBare("check", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			// The plain form deliberately constructs no network, cache, trust, or
			// configuration dependencies. Its entire scope is the two project files.
			manifest, lock := projectPaths(current)
			result, err := application.New(manifest, lock, nil, nil, nil).Check(ctx, application.CheckOptions{})
			return result, runner.stdoutPalette.Good("The manifest and lock file agree."), err
		}),
	}
}

// checkUpstreamCommand checks selected origins without making the offline
// check's network dependencies implicit.
func (runner *runner) checkUpstreamCommand() *urfave.Command {
	flags := append(runner.networkFlags(true),
		&urfave.BoolFlag{Name: "verify", Usage: "Download and hash each selected asset instead of checking metadata."},
	)
	return &urfave.Command{
		Name:      "upstream",
		Usage:     "Check selected upstream metadata, or their locked bytes with --verify.",
		ArgsUsage: "[<namespace>/<name>@<version>...]",
		Flags:     flags,
		Action: runner.run("check.upstream", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			assets, err := checkCoordinates(current)
			if err != nil {
				return nil, "", err
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
			mode := application.CheckMetadata
			if current.Bool("verify") {
				mode = application.CheckBytes
			}
			result, err := service.Check(ctx, application.CheckOptions{
				Concurrency: concurrency,
				MaxSize:     maxSize,
				Mode:        mode,
				Assets:      assets,
			})
			return result, checkUpstreamText(runner, result), err
		}),
	}
}

// checkCoordinates accepts only complete coordinates because origin checks are
// precise operations, rather than the version-group selection used by pull.
func checkCoordinates(current *urfave.Command) ([]coord.Coordinate, error) {
	values := current.Args().Slice()
	assets := make([]coord.Coordinate, 0, len(values))
	for _, value := range values {
		asset, err := parseCoordinate(value)
		if err != nil {
			return nil, err
		}
		assets = append(assets, asset)
	}
	return assets, nil
}

func checkUpstreamText(runner *runner, result application.CheckResult) string {
	count := runner.stdoutPalette.Strong(plural(result.AssetCount, "asset"))
	if result.AssetCount < result.ProjectCount {
		count = fmt.Sprintf("%s of %d assets", runner.stdoutPalette.Strong(plural(result.AssetCount, "asset")), result.ProjectCount)
	}
	if result.Verified {
		return fmt.Sprintf("The upstream bytes match the lock for %s. Downloaded %s for verification.",
			count, runner.stdoutPalette.Strong(plural(result.Downloaded, "asset")))
	}
	return fmt.Sprintf("The upstream metadata matches the lock for %s.", count)
}
