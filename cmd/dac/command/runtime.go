package command

import (
	"context"
	"fmt"
	"strings"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/fault"
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
		return project.Paths{}, fault.NewFilesystemError(err)
	}
	return project.Discover(directory)
}

// calculatePin drives the trust-on-first-use download with dac's progress
// presentation. The retrieve-and-discard protocol itself belongs to asset.
func (runtime *runtime) calculatePin(ctx context.Context, paths project.Paths, resolved manifest.ResolvedAsset) (string, error) {
	downloads, err := paths.OpenDownloads()
	if err != nil {
		return "", err
	}
	defer func() { _ = downloads.Close() }()
	var digest string
	_, err = runtime.Output.WithDownloadProgress(ctx, resolved.ResolvedFile, resolved.ResolvedURL, func(ctx context.Context) error {
		var digestErr error
		digest, digestErr = runtime.Downloader.CalculateDigest(ctx, downloads.Root(), assetRequest(resolved))
		return digestErr
	})
	if err != nil {
		return "", err
	}
	return digest, nil
}

func assetRequest(resolved manifest.ResolvedAsset) asset.Request {
	return asset.Request{Name: resolved.Name, URL: resolved.ResolvedURL, File: resolved.ResolvedFile, Headers: resolved.Headers, Policy: resolved.Transfer}
}

// assignments parses repeated NAME=VALUE options into the spelling the user
// supplied. identity maps a name onto the key that decides duplication, which
// is exact for manifest variables and case-insensitive for HTTP headers.
func assignments(values []string, identity func(string) string, malformed, duplicate error) (map[string]string, error) {
	result := make(map[string]string, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		name, item, found := strings.Cut(value, "=")
		if !found || name == "" {
			return nil, malformed
		}
		key := identity(name)
		if seen[key] {
			return nil, fmt.Errorf("%w %q", duplicate, name)
		}
		seen[key] = true
		result[name] = item
	}
	return result, nil
}

// names parses repeated bare-name options, mapping each duplication key to the
// spelling the user supplied so errors can quote their input back to them.
func names(values []string, identity func(string) string, duplicate error) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for _, value := range values {
		key := identity(value)
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("%w %q", duplicate, value)
		}
		result[key] = value
	}
	return result, nil
}

// exactly is the duplication rule for names compared byte for byte.
func exactly(name string) string { return name }

// parseSets validates repeated KEY=VALUE flags without allowing a later flag
// to silently override an earlier one.
func parseSets(values []string) (map[string]string, error) {
	result, err := assignments(values, exactly, ErrInvalidSet, ErrDuplicateSet)
	if err != nil {
		return nil, err
	}
	for key := range result {
		if !manifest.ValidVariableName(key) {
			return nil, fmt.Errorf("invalid variable name %q", key)
		}
	}
	return result, nil
}

// parseHeaders preserves user-facing spelling while enforcing HTTP's
// case-insensitive header identity.
func parseHeaders(values []string) (map[string]string, error) {
	result, err := assignments(values, asset.HeaderIdentity, ErrHeaderFormat, ErrDuplicateHeader)
	if err != nil {
		return nil, err
	}
	if err := asset.ValidateHeaders(result); err != nil {
		return nil, err
	}
	return result, nil
}

// parseVariableNames validates the removal half of update's set algebra.
func parseVariableNames(values []string) (map[string]string, error) {
	result, err := names(values, exactly, ErrDuplicateSet)
	if err != nil {
		return nil, err
	}
	for key := range result {
		if !manifest.ValidVariableName(key) {
			return nil, fmt.Errorf("invalid variable name %q", key)
		}
	}
	return result, nil
}

// parseHeaderNames returns normalized lookup keys and original spellings for
// useful errors when update removes configured headers.
func parseHeaderNames(values []string) (map[string]string, error) {
	result, err := names(values, asset.HeaderIdentity, ErrDuplicateHeader)
	if err != nil {
		return nil, err
	}
	for _, spelling := range result {
		if err := asset.ValidateHeaderName(spelling); err != nil {
			return nil, err
		}
	}
	return result, nil
}
