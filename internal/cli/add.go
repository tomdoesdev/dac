package cli

import (
	"context"
	"fmt"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/httpclient"
	"github.com/tomdoesdev/dac/internal/output/style"
)

func (runner *runner) addCommand() *urfave.Command {
	flags := append(runner.networkFlags(false),
		&urfave.StringFlag{Name: "integrity", Usage: "Require this sha256 digest."},
		&urfave.StringFlag{Name: "name", Usage: "Call the asset this instead of the name its origin gives."},
		&urfave.BoolFlag{Name: "pin", Usage: "Record the resolved digest as the asset integrity value."},
		&urfave.BoolFlag{Name: "force", Usage: "Replace the source of an asset version the manifest already has."},
		&urfave.BoolFlag{Name: "offline", Usage: "Refuse network access, including the request required by --pin."},
	)
	return &urfave.Command{
		Name:      "add",
		Usage:     "Add one asset to the manifest without changing the lock file.",
		ArgsUsage: "<namespace>/<name>@<version> <url>",
		Flags:     flags,
		Action: runner.run("add", func(ctx context.Context, current *urfave.Command) (any, string, error) {
			name, source, err := coordinateAndSource(current)
			if err != nil {
				return nil, "", err
			}
			service := runner.projectService(current)
			var maxSize int64
			if current.Bool("pin") && !current.Bool("offline") {
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
				Coordinate: name,
				URL:        source,
				Integrity:  current.String("integrity"),
				Filename:   current.String("name"),
				Force:      current.Bool("force"),
				Pin:        current.Bool("pin"),
				MaxSize:    maxSize,
				Offline:    current.Bool("offline"),
			})
			if err != nil {
				return nil, "", err
			}
			return result, addText(runner.stdoutPalette, name, result), nil
		}),
	}
}

// addText summarizes one manifest addition and the digest observed by --pin.
func addText(palette style.Palette, name coord.Coordinate, result application.AddResult) string {
	var text strings.Builder
	_, _ = fmt.Fprintf(&text, "Added %s", palette.Name(name.String()))
	if result.Digest != "" {
		// The digest is secondary unless the caller wants to copy it into the manifest.
		_, _ = fmt.Fprintf(&text, " (%s)", palette.Detail(result.Digest))
	}
	if result.Integrity != "" {
		text.WriteString(", pinned")
	}
	text.WriteByte('.')
	if len(result.Siblings) > 0 {
		_, _ = fmt.Fprintf(&text, " %s also has %s.",
			palette.Name(name.Group().String()), palette.Name(strings.Join(result.Siblings, ", ")))
	}
	if len(result.SharedSources) > 0 {
		_, _ = fmt.Fprintf(&text, " %s",
			palette.Warn(fmt.Sprintf("Warning: %s shares this source URL, and one URL serves one set of bytes.",
				strings.Join(result.SharedSources, ", "))))
	}
	return text.String()
}
