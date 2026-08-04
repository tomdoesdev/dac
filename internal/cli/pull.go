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
		Usage: "Install all missing locked assets.",
		Flags: flags,
		Action: runner.run("pull", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
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
			result, err := service.Pull(ctx, application.NetworkOptions{
				Concurrency: concurrency,
				MaxSize:     maxSize,
				Offline:     current.Bool("offline"),
			})
			return result, fmt.Sprintf("Pulled %s.", plural(result.AssetCount, "asset")), err
		}),
	}
}
