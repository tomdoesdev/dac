package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/output"
	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/kit/cli"
)

// Dependencies are the process-scoped concrete services shared by commands.
type Dependencies struct {
	Output     *output.Writer
	Downloader *asset.Downloader
	CWD        func() (string, error)
}

type runtime struct{ Dependencies }

// Register installs dac's complete command surface on app.
func Register(app *cli.App, dependencies Dependencies) {
	runtime := &runtime{Dependencies: dependencies}
	app.MustAddCommand("init", &initCommand{runtime: runtime})
	app.MustAddCommand("add <name> <url>", &addCommand{runtime: runtime})
	app.MustAddCommand("update <name>", &updateCommand{runtime: runtime})
	app.MustAddCommand("lock [names...]", &lockCommand{runtime: runtime})
	app.MustAddCommand("pull", &pullCommand{runtime: runtime})
}

func (runtime *runtime) project() (project.Paths, error) {
	directory, err := runtime.CWD()
	if err != nil {
		return project.Paths{}, project.NewError("filesystem", err)
	}
	return project.Discover(directory)
}

// calculatePin retrieves and hashes an asset without installing or accepting
// its bytes. Both add and update use this trust-on-first-use workflow.
func (runtime *runtime) calculatePin(ctx context.Context, paths project.Paths, resolved manifest.ResolvedAsset) (string, error) {
	downloads, err := paths.OpenDownloads()
	if err != nil {
		return "", err
	}
	defer func() { _ = downloads.Close() }()
	download, err := runtime.Downloader.Download(ctx, downloads, assetRequest(resolved), "")
	if err != nil {
		return "", err
	}
	digest := download.Digest
	if err := download.Discard(); err != nil {
		return "", project.NewError("filesystem", err)
	}
	return digest, nil
}

func assetRequest(resolved manifest.ResolvedAsset) asset.Request {
	return asset.Request{Name: resolved.Name, URL: resolved.ResolvedURL, File: resolved.ResolvedFile, Headers: resolved.Headers}
}

func parseSets(values []string) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for _, value := range values {
		key, item, found := strings.Cut(value, "=")
		if !found || key == "" {
			return nil, fmt.Errorf("--set must be KEY=VALUE")
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("--set repeats key %q", key)
		}
		result[key] = item
	}
	return result, nil
}

func findResolved(values []manifest.ResolvedAsset, name string) manifest.ResolvedAsset {
	for _, value := range values {
		if value.Name == name {
			return value
		}
	}
	return manifest.ResolvedAsset{}
}
