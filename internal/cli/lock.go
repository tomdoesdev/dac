package cli

import (
	"context"
	"fmt"

	urfave "github.com/urfave/cli/v3"

	"github.com/tom/dac/internal/application"
)

func (runner *runner) lockCommand() *urfave.Command {
	flags := append(runner.networkFlags(true, true),
		&urfave.BoolFlag{Name: "refresh", Usage: "Contact the origin even when the cache satisfies an integrity value."},
		&urfave.BoolFlag{Name: "check", Usage: "Report whether the lock file is current without writing it."},
	)
	return &urfave.Command{
		Name:  "lock",
		Usage: "Resolve all manifest assets.",
		Flags: flags,
		Action: runner.run("lock", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
				return nil, "", err
			}
			service, client, err := runner.networkService(current, false)
			if err != nil {
				return nil, "", err
			}
			defer client.Close()
			maxSize, err := maximumSize(current)
			if err != nil {
				return nil, "", err
			}
			concurrency, err := concurrency(current)
			if err != nil {
				return nil, "", err
			}
			check := current.Bool("check")
			result, err := service.Lock(ctx, application.NetworkOptions{
				Concurrency: concurrency,
				MaxSize:     maxSize,
				Refresh:     current.Bool("refresh"),
				Check:       check,
			})
			if check {
				return result, fmt.Sprintf("The lock file is current for %s.", plural(result.AssetCount, "asset")), err
			}
			return result, fmt.Sprintf("Locked %s.", plural(result.AssetCount, "asset")), err
		}),
	}
}
