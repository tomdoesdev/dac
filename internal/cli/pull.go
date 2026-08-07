package cli

import (
	"context"
	"fmt"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/dac"
	"github.com/tomdoesdev/dac/internal/output/style"
)

func (runner *runner) pullCommand() *urfave.Command {
	flags := append(runner.networkFlags(true),
		&urfave.BoolFlag{Name: "offline", Usage: "Disable network requests."},
		&urfave.BoolFlag{Name: "refresh", Usage: "Resolve the assets against their origins and rewrite the lock file around what they now serve."},
	)
	return &urfave.Command{
		Name: "pull",
		// Pull is the reconciliation boundary as well as the cache installation command.
		Usage: "Reconcile lock state and install the locked assets, or the ones named.",
		// Naming assets narrows what is fetched, for the job that needs one of them.
		ArgsUsage: "[<namespace>/<name>[@<version>]...]",
		Flags:     flags,
		Action: runner.run("pull", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			assets, err := selections(current)
			if err != nil {
				return nil, "", err
			}
			service, client, err := runner.networkService(ctx, current, runner.json)
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
			result, err := service.Pull(ctx, dac.PullOptions{
				Concurrency: concurrency,
				MaxSize:     maxSize,
				Offline:     current.Bool("offline"),
				Refresh:     current.Bool("refresh"),
				Assets:      assets,
			})
			return result, pullText(runner.stdoutPalette, result), err
		}),
	}
}

// pullText summarizes one pull, and the lock file work in front of it when there was any.
// A narrowed pull says what it left alone.
func pullText(palette style.Palette, result dac.PullResult) string {
	text := lockText(palette, result)
	if result.AssetCount < result.ProjectCount {
		return text + fmt.Sprintf("Pulled %s of %d.",
			palette.Strong(plural(result.AssetCount, "asset")), result.ProjectCount)
	}
	return text + fmt.Sprintf("Pulled %s.", palette.Strong(plural(result.AssetCount, "asset")))
}

// lockText summarizes the lock file a pull settled, and is empty for the pull that settled none.
// Whether the file moved is reported separately from what was resolved, because the two come apart in both directions.
func lockText(palette style.Palette, result dac.PullResult) string {
	switch {
	case len(result.Locked) > 0 && result.Changed:
		return fmt.Sprintf("Locked %s. ", palette.Name(strings.Join(result.Locked, ", ")))
	case len(result.Locked) > 0:
		return fmt.Sprintf("Resolved %s. The lock file is unchanged. ",
			palette.Name(strings.Join(result.Locked, ", ")))
	case result.Changed:
		return fmt.Sprintf("Updated the lock file for %s without a request. ",
			palette.Strong(plural(result.ProjectCount, "asset")))
	}
	return ""
}
