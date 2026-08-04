package cli

import (
	"context"
	"fmt"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
)

func (runner *runner) pullCommand() *urfave.Command {
	flags := append(runner.networkFlags(true),
		&urfave.BoolFlag{Name: "offline", Usage: "Disable network requests."},
	)
	return &urfave.Command{
		Name: "pull",
		// A pull installs what the lock file already says. It writes no project
		// files at all: settling a manifest the lock no longer describes is
		// dac lock, and checking the origins without writing is dac verify
		// --refresh.
		Usage: "Install missing locked assets, or the ones named.",
		// Naming assets narrows what is fetched, for the job that needs one of
		// them. It does not narrow what is checked: the lock file still has to
		// describe the manifest either way.
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
				NetworkOptions: application.NetworkOptions{
					Concurrency: concurrency,
					MaxSize:     maxSize,
					Offline:     current.Bool("offline"),
				},
				Assets: assets,
			})
			return result, pullText(result), err
		}),
	}
}

// pullText summarizes one pull.
//
// A narrowed pull says what it left alone. "Pulled 1 asset" is true of a
// project with one asset and of a job that asked for one of twenty, and the
// difference is the whole reason to name assets in the first place.
func pullText(result application.PullResult) string {
	if result.AssetCount < result.ProjectCount {
		return fmt.Sprintf("Pulled %s of %d.", plural(result.AssetCount, "asset"), result.ProjectCount)
	}
	return fmt.Sprintf("Pulled %s.", plural(result.AssetCount, "asset"))
}
