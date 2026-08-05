package cli

import (
	"context"
	"fmt"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/style"
)

// lockCommand builds the lock file command.
// It takes no --offline flag.
func (runner *runner) lockCommand() *urfave.Command {
	flags := append(runner.networkFlags(true),
		&urfave.BoolFlag{Name: "refresh", Usage: "Resolve every manifest asset against its origin instead of only the ones the lock file does not describe."},
	)
	return &urfave.Command{
		Name:  "lock",
		Usage: "Resolve the manifest assets the lock file does not describe and write it.",
		Flags: flags,
		Action: runner.run("lock", func(ctx context.Context, current *urfave.Command) (any, string, error) {
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
			result, err := service.Lock(ctx, application.LockOptions{
				Concurrency: concurrency,
				MaxSize:     maxSize,
				Refresh:     current.Bool("refresh"),
			})
			return result, lockText(runner.stdoutPalette, result), err
		}),
	}
}

// lockText summarizes one lock file update.
// Whether the file moved is reported separately from what was resolved, because the two come apart in both directions.
func lockText(palette style.Palette, result application.LockResult) string {
	if !result.Changed {
		if len(result.Locked) > 0 {
			return fmt.Sprintf("Resolved %s. The lock file is unchanged.",
				palette.Name(strings.Join(result.Locked, ", ")))
		}
		return fmt.Sprintf("The lock file already describes %s.",
			palette.Strong(plural(result.AssetCount, "asset")))
	}
	if len(result.Locked) > 0 {
		return fmt.Sprintf("Locked %s.", palette.Name(strings.Join(result.Locked, ", ")))
	}
	return fmt.Sprintf("Updated the lock file for %s without a request.",
		palette.Strong(plural(result.AssetCount, "asset")))
}
