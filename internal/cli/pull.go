package cli

import (
	"context"
	"fmt"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/style"
)

func (runner *runner) pullCommand() *urfave.Command {
	flags := append(runner.networkFlags(true),
		&urfave.BoolFlag{Name: "offline", Usage: "Disable network requests."},
		&urfave.BoolFlag{Name: "refresh", Usage: "Resolve the assets against their origins and rewrite the lock file around what they now serve."},
	)
	return &urfave.Command{
		Name: "pull",
		// A pull installs what the lock file already says, and writes the lock file when the project has none yet.
		Usage: "Install the locked assets, or the ones named, writing the lock file if there is none.",
		// Naming assets narrows what is fetched, for the job that needs one of them.
		ArgsUsage:     "[<namespace>/<name>[@<version>]...]",
		Flags:         flags,
		ShellComplete: runner.completeCoordinates(),
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
			result, err := service.Pull(ctx, application.PullOptions{
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
func pullText(palette style.Palette, result application.PullResult) string {
	text := lockText(palette, result)
	if result.AssetCount < result.ProjectCount {
		return text + fmt.Sprintf("Pulled %s of %d.",
			palette.Strong(plural(result.AssetCount, "asset")), result.ProjectCount)
	}
	return text + fmt.Sprintf("Pulled %s.", palette.Strong(plural(result.AssetCount, "asset")))
}

// lockText summarizes the lock file a pull settled, and is empty for the pull that settled none.
// Whether the file moved is reported separately from what was resolved, because the two come apart in both directions.
func lockText(palette style.Palette, result application.PullResult) string {
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
