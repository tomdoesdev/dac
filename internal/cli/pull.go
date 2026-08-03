package cli

import (
	"context"
	"fmt"

	urfave "github.com/urfave/cli/v3"

	"github.com/tom/dac/internal/application"
)

func (runner *runner) pullCommand() *urfave.Command {
	flags := append(runner.networkFlags(true, false),
		&urfave.BoolFlag{Name: "offline", Usage: "Disable network requests."},
		&urfave.StringFlag{Name: "distdir", Sources: urfave.EnvVars("DAC_DISTDIR"), Usage: "Install locked assets from this directory before requesting them."},
	)
	return &urfave.Command{
		Name:  "pull",
		Usage: "Install all missing locked assets.",
		Flags: flags,
		Action: runner.run("pull", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
				return nil, "", err
			}
			service, client, err := runner.networkService(current, runner.json)
			if err != nil {
				return nil, "", err
			}
			defer client.Close()
			concurrency, err := concurrency(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.Pull(ctx, application.NetworkOptions{
				Concurrency: concurrency,
				Offline:     current.Bool("offline"),
				DistDir:     current.String("distdir"),
			})
			return result, fmt.Sprintf("Pulled %s.", plural(result.AssetCount, "asset")), err
		}),
	}
}
