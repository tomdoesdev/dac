package cli

import (
	"context"
	"fmt"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/fault"
)

func (runner *runner) pullCommand() *urfave.Command {
	flags := append(runner.networkFlags(true, true),
		&urfave.BoolFlag{Name: "update-lock", Usage: "Resolve the manifest assets the lock file does not describe and write it."},
		&urfave.BoolFlag{Name: "refresh-lock", Usage: "Resolve every manifest asset against its origin and write the lock file."},
		&urfave.BoolFlag{Name: "rebind", Usage: "Accept origins that no longer serve the bytes a locked version names."},
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
			// A refresh is an update that declines the shortcuts, so it implies
			// one. Resolving every origin without writing what it finds is the
			// drift check, which is dac verify --refresh.
			refresh := current.Bool("refresh-lock")
			updateLock := current.Bool("update-lock") || refresh
			offline := current.Bool("offline")
			if updateLock && offline {
				return nil, "", fault.New("invalid_arguments", "Offline mode cannot update the lock file, because it resolves no bytes to lock.")
			}
			// Rebinding is a decision about what a version means, and only a
			// command that writes the lock file can act on it. Accepting the
			// flag on a pull that cannot write would say DAC had considered
			// the request when nothing had asked the question.
			rebind := current.Bool("rebind")
			if rebind && !updateLock {
				return nil, "", fault.New("invalid_arguments", "Rebinding a version writes the lock file. Use --rebind with --update-lock or --refresh-lock.")
			}
			service, client, err := runner.networkService(ctx, current, runner.json)
			if err != nil {
				return nil, "", err
			}
			defer client.Close()
			concurrency, err := concurrency(current)
			if err != nil {
				return nil, "", err
			}
			maxSize, err := maximumSize(current)
			if err != nil {
				return nil, "", err
			}
			result, err := service.Pull(ctx, application.NetworkOptions{
				Concurrency: concurrency,
				MaxSize:     maxSize,
				Offline:     offline,
				DistDir:     current.String("distdir"),
				UpdateLock:  updateLock,
				Refresh:     refresh,
				AllowRebind: rebind,
			})
			return result, pullText(result), err
		}),
	}
}

// pullText summarizes one pull. It reports lock work separately from install
// work and names the assets, because an operator who asked for --update-lock is
// about to review that file's diff and wants to know what to expect in it.
func pullText(result application.PullResult) string {
	text := fmt.Sprintf("Pulled %s.", plural(result.AssetCount, "asset"))
	if len(result.Locked) > 0 {
		text += fmt.Sprintf(" Locked %s.", strings.Join(result.Locked, ", "))
	}
	return text
}
