package cli

import (
	"context"
	"fmt"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/httpclient"
)

func (runner *runner) addCommand() *urfave.Command {
	flags := append(runner.networkFlags(false, true),
		&urfave.StringFlag{Name: "source", Required: true, Usage: "Fetch the asset from this URL."},
		&urfave.StringFlag{Name: "integrity", Usage: "Require this sha256 digest."},
		&urfave.BoolFlag{Name: "pin", Usage: "Record the resolved digest as the asset integrity value."},
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
				Pin:               current.Bool("pin"),
				MaxSize:           maxSize,
				Offline:           current.Bool("offline"),
			})
			if err != nil {
				return nil, "", err
			}
			return result, addText(name, version, result), nil
		}),
	}
}

// addText summarizes one addition.
//
// It reports the resolved digest, which is what an operator pastes back into
// the manifest when they want to pin an asset by hand, and it names any
// coordinate the addition retired, because DAC keeps one version of each asset
// name and a silent replacement breaks every reference to the old one.
func addText(name, version string, result application.AddResult) string {
	var text strings.Builder
	if result.Replaced != "" {
		_, _ = fmt.Fprintf(&text, "Replaced %s with %s@%s", result.Replaced, name, version)
	} else {
		_, _ = fmt.Fprintf(&text, "Added %s@%s", name, version)
	}
	if result.Digest != "" {
		_, _ = fmt.Fprintf(&text, " (%s)", result.Digest)
	}
	if result.Integrity != "" {
		text.WriteString(", pinned")
	}
	text.WriteByte('.')
	return text.String()
}
