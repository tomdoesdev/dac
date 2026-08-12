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
	app.MustAddCommand("add <name> <url>", newAddCommand(runtime))
	app.MustAddCommand("update <name>", newUpdateCommand(runtime))
	app.MustAddCommand("lock [names...]", &lockCommand{runtime: runtime})
	app.MustAddCommand("pull", &pullCommand{runtime: runtime})
	app.MustAddCommand("status", &statusCommand{runtime: runtime})
}

func (runtime *runtime) project() (project.Paths, error) {
	directory, err := runtime.CWD()
	if err != nil {
		return project.Paths{}, project.NewFilesystemError(err)
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
	var download *asset.StagedDownload
	err = runtime.Output.WithDownloadProgress(ctx, resolved.Name, resolved.ResolvedFile, resolved.ResolvedURL, func(ctx context.Context) error {
		download, err = runtime.Downloader.Download(ctx, downloads, assetRequest(resolved), "")
		return err
	})
	if err != nil {
		return "", err
	}
	digest := download.Digest
	if err := download.Discard(); err != nil {
		return "", project.NewFilesystemError(err)
	}
	return digest, nil
}

func assetRequest(resolved manifest.ResolvedAsset) asset.Request {
	return asset.Request{Name: resolved.Name, URL: resolved.ResolvedURL, File: resolved.ResolvedFile, Headers: resolved.Headers, Policy: resolved.Transfer}
}

// parseSets validates repeated KEY=VALUE flags without allowing a later flag
// to silently override an earlier one.
func parseSets(values []string) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for _, value := range values {
		key, item, found := strings.Cut(value, "=")
		if !found || key == "" {
			return nil, ErrInvalidSet
		}
		if !manifest.ValidVariableName(key) {
			return nil, fmt.Errorf("invalid variable name %q", key)
		}
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("%w %q", ErrDuplicateSet, key)
		}
		result[key] = item
	}
	return result, nil
}

// parseHeaders preserves user-facing spelling while enforcing HTTP's
// case-insensitive header identity.
func parseHeaders(values []string) (map[string]string, error) {
	result := make(map[string]string, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		key, item, found := strings.Cut(value, "=")
		if !found || key == "" {
			return nil, ErrHeaderFormat
		}
		normalized := strings.ToLower(key)
		if seen[normalized] {
			return nil, fmt.Errorf("%w %q", ErrDuplicateHeader, key)
		}
		seen[normalized] = true
		result[key] = item
	}
	if err := asset.ValidateHeaders(result); err != nil {
		return nil, err
	}
	return result, nil
}

// parseVariableNames validates the removal half of update's set algebra.
func parseVariableNames(values []string) (map[string]bool, error) {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		if !manifest.ValidVariableName(value) {
			return nil, fmt.Errorf("invalid variable name %q", value)
		}
		if result[value] {
			return nil, fmt.Errorf("%w %q", ErrDuplicateSet, value)
		}
		result[value] = true
	}
	return result, nil
}

// parseHeaderNames returns normalized lookup keys and original spellings for
// useful errors when update removes configured headers.
func parseHeaderNames(values []string) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for _, value := range values {
		if err := asset.ValidateHeaders(map[string]string{value: ""}); err != nil {
			return nil, err
		}
		normalized := strings.ToLower(value)
		if _, exists := result[normalized]; exists {
			return nil, fmt.Errorf("%w %q", ErrDuplicateHeader, value)
		}
		result[normalized] = value
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
