package cli

import (
	"context"
	"fmt"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/bytesize"
)

// infoCommand defines the project inspection command and its coordinate filter.
func (runner *runner) infoCommand() *urfave.Command {
	return &urfave.Command{
		Name:      "info",
		Usage:     "Show project asset and request information.",
		ArgsUsage: "[<name>@<version>]",
		Action: runner.run("info", func(_ context.Context, current *urfave.Command) (any, string, error) {
			name, version, err := optionalCoordinate(current)
			if err != nil {
				return nil, "", err
			}
			service, err := runner.storeService(current)
			if err != nil {
				return nil, "", err
			}
			config, err := loadRewriteConfig(service.ManifestPath, false)
			if err != nil {
				return nil, "", err
			}
			result, err := service.Info(application.InfoOptions{Name: name, Version: version, Rewriter: config})
			return result, infoText(result), err
		}),
	}
}

// infoText formats each asset as one detailed information block.
func infoText(result application.InfoResult) string {
	if len(result.Assets) == 0 {
		return "No assets."
	}
	var text strings.Builder
	for index, asset := range result.Assets {
		if index > 0 {
			text.WriteByte('\n')
		}
		_, _ = fmt.Fprintf(&text,
			"%s@%s\nsource: %s\nrequest: %s\npolicy: %s\nlock: %s\ncache: %s\n",
			asset.Name, asset.Version, asset.SourceURL, asset.RequestURL,
			asset.RequestStatus, result.LockStatus, asset.CacheStatus)
		if asset.Integrity != "" {
			_, _ = fmt.Fprintf(&text, "integrity: %s\n", asset.Integrity)
		}
		if asset.Digest != "" {
			_, _ = fmt.Fprintf(&text, "digest: %s\n", asset.Digest)
		}
		if asset.Size != nil {
			_, _ = fmt.Fprintf(&text, "size: %s\n", bytesize.Format(*asset.Size))
		}
		if asset.Path != "" {
			_, _ = fmt.Fprintf(&text, "path: %s\n", asset.Path)
		}
	}
	return strings.TrimSuffix(text.String(), "\n")
}
