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
//
// It takes no --offline flag. Locking an asset means resolving it, and resolving
// it means fetching the bytes to write a digest for, so an offline lock is a
// command with nothing to do. Use add --offline to record a source now and
// dac lock to resolve it later.
func (runner *runner) lockCommand() *urfave.Command {
	flags := append(runner.networkFlags(true),
		&urfave.BoolFlag{Name: "refresh", Usage: "Resolve every manifest asset against its origin instead of only the ones the lock file does not describe."},
		&urfave.BoolFlag{Name: "rebind", Usage: "Accept origins that no longer serve the bytes a locked version names."},
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
				AllowRebind: current.Bool("rebind"),
			})
			return result, lockText(runner.stdoutPalette, result), err
		}),
	}
}

// lockText summarizes one lock file update. It names the assets it resolved,
// because an operator who ran this is about to review that file's diff and wants
// to know what to expect in it.
//
// Whether the file moved is reported separately from what was resolved, because
// the two come apart in both directions. A --refresh resolves every asset and
// usually finds nothing to write, so announcing what it locked would describe a
// diff that is not there. A lock file can also be rewritten with nothing
// resolved at all -- a file name backfilled from the URL, an ETag dropped
// because the manifest pinned the asset -- and reporting that as no work would
// deny a change the operator is about to find in git.
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
