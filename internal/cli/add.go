package cli

import (
	"context"
	"fmt"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/coord"
	"github.com/tom/dac/internal/httpclient"
)

func (runner *runner) addCommand() *urfave.Command {
	flags := append(runner.networkFlags(false),
		&urfave.StringFlag{Name: "integrity", Usage: "Require this sha256 digest."},
		&urfave.BoolFlag{Name: "pin", Usage: "Record the resolved digest as the asset integrity value."},
		&urfave.BoolFlag{Name: "allow-insecure-http", Usage: "Permit a non-local HTTP URL."},
		&urfave.BoolFlag{Name: "force", Usage: "Replace the source of an asset version the manifest already has."},
		&urfave.BoolFlag{Name: "rebind", Usage: "Accept a source that serves different bytes than this version is locked to."},
		&urfave.BoolFlag{Name: "offline", Usage: "Write only the manifest without network access."},
	)
	return &urfave.Command{
		Name:      "add",
		Usage:     "Add one asset and update the project files.",
		ArgsUsage: "<namespace>/<name>@<version> <url>",
		Flags:     flags,
		Action: runner.run("add", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			name, source, err := coordinateAndSource(current)
			if err != nil {
				return nil, "", err
			}
			service := runner.projectService(current)
			var maxSize int64
			if !current.Bool("offline") {
				var client *httpclient.Client
				service, client, err = runner.networkService(ctx, current, false)
				if err != nil {
					return nil, "", err
				}
				defer client.Close()
				maxSize, err = runner.maximumSize(current)
				if err != nil {
					return nil, "", err
				}
			}
			result, err := service.Add(ctx, application.AddOptions{
				Coordinate:        name,
				URL:               source,
				Integrity:         current.String("integrity"),
				AllowInsecureHTTP: current.Bool("allow-insecure-http"),
				Force:             current.Bool("force"),
				Pin:               current.Bool("pin"),
				MaxSize:           maxSize,
				Offline:           current.Bool("offline"),
				AllowRebind:       current.Bool("rebind"),
			})
			if err != nil {
				return nil, "", err
			}
			return result, addText(name, result), nil
		}),
	}
}

// addText summarizes one addition.
//
// It reports the resolved digest, which is what an operator pastes back into
// the manifest when they want to pin an asset by hand, and it names the other
// versions of the asset the project now carries. An add no longer retires the
// version before it, so the count of things this project fetches went up, and
// the command that put it up should be the one to mention it.
//
// It also names the assets the addition locked on the way past. Adding one
// asset to a project whose manifest someone hand edited settles the rest too,
// and an operator reviewing the lock file diff should read about that here
// rather than infer it from the diff.
func addText(name coord.Coordinate, result application.AddResult) string {
	var text strings.Builder
	_, _ = fmt.Fprintf(&text, "Added %s", name)
	if result.Digest != "" {
		_, _ = fmt.Fprintf(&text, " (%s)", result.Digest)
	}
	if result.Integrity != "" {
		text.WriteString(", pinned")
	}
	text.WriteByte('.')
	if len(result.Siblings) > 0 {
		_, _ = fmt.Fprintf(&text, " %s also has %s.", name.Group(), strings.Join(result.Siblings, ", "))
	}
	if len(result.SharedSources) > 0 {
		_, _ = fmt.Fprintf(&text, " Warning: %s shares this source URL, and one URL serves one set of bytes.",
			strings.Join(result.SharedSources, ", "))
	}
	if len(result.Locked) > 0 {
		_, _ = fmt.Fprintf(&text, " Locked %s.", strings.Join(result.Locked, ", "))
	}
	return text.String()
}
