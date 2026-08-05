package cli

import (
	"context"
	"fmt"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/style"
)

func (runner *runner) pullCommand() *urfave.Command {
	flags := append(runner.networkFlags(true),
		&urfave.BoolFlag{Name: "offline", Usage: "Disable network requests."},
	)
	return &urfave.Command{
		Name: "pull",
		// A pull installs what the lock file already says.
		Usage: "Install missing locked assets, or the ones named.",
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
				Assets:      assets,
			})
			return result, pullText(runner.stdoutPalette, result), err
		}),
	}
}

// pullText summarizes one pull.
// A narrowed pull says what it left alone.
func pullText(palette style.Palette, result application.PullResult) string {
	if result.AssetCount < result.ProjectCount {
		return fmt.Sprintf("Pulled %s of %d.",
			palette.Strong(plural(result.AssetCount, "asset")), result.ProjectCount)
	}
	return fmt.Sprintf("Pulled %s.", palette.Strong(plural(result.AssetCount, "asset")))
}
