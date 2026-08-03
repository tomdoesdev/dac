package cli

import (
	"context"
	"fmt"

	urfave "github.com/urfave/cli/v3"

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/httpclient"
)

func (runner *runner) addCommand() *urfave.Command {
	flags := append(runner.networkFlags(false, true),
		&urfave.StringFlag{Name: "source", Required: true, Usage: "Fetch the asset from this URL."},
		&urfave.StringFlag{Name: "integrity", Usage: "Require this sha256 digest."},
		&urfave.BoolFlag{Name: "allow-insecure-http", Usage: "Permit a non-local HTTP URL."},
		&urfave.BoolFlag{Name: "force", Usage: "Replace an existing asset."},
		&urfave.BoolFlag{Name: "offline", Usage: "Write only the manifest without network access."},
	)
	return &urfave.Command{
		Name:      "add",
		Usage:     "Add one asset and update the project files.",
		ArgsUsage: "<name>@<version>",
		Flags:     flags,
		Action: runner.run("add", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			name, version, err := coordinate(current)
			if err != nil {
				return nil, "", err
			}
			service := runner.projectService(current)
			var maxSize int64
			if !current.Bool("offline") {
				var client *httpclient.Client
				service, client, err = runner.networkService(current, false)
				if err != nil {
					return nil, "", err
				}
				defer client.Close()
				maxSize, err = maximumSize(current)
				if err != nil {
					return nil, "", err
				}
			}
			result, err := service.Add(ctx, application.AddOptions{
				Name:              name,
				Version:           version,
				URL:               current.String("source"),
				Integrity:         current.String("integrity"),
				AllowInsecureHTTP: current.Bool("allow-insecure-http"),
				Force:             current.Bool("force"),
				MaxSize:           maxSize,
				Offline:           current.Bool("offline"),
			})
			if err != nil {
				return nil, "", err
			}
			// Report the digest DAC resolved so it can be pasted straight into
			// the manifest as an integrity value.
			if result.Digest != "" {
				return result, fmt.Sprintf("Added %s@%s (%s).", name, version, result.Digest), nil
			}
			return result, fmt.Sprintf("Added %s@%s.", name, version), nil
		}),
	}
}
