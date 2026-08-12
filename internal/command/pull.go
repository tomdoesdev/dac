package command

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/lockfile"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/output"
	"github.com/tomdoesdev/dac/internal/project"
)

type pullCommand struct {
	runtime *runtime
	Offline bool `flag:"offline" help:"verify local downloads without network access"`
	Force   bool `flag:"force" help:"re-download every locked artifact"`
}

func (*pullCommand) Description() string { return "Reproduce verified files from dac.lock" }
func (command *pullCommand) Validate() error {
	if err := command.runtime.Output.ValidateOptions(); err != nil {
		return err
	}
	if command.Offline && command.Force {
		return errors.New("--offline and --force cannot be combined")
	}
	return nil
}

func (command *pullCommand) Run(ctx context.Context) error {
	paths, err := command.runtime.project()
	if err != nil {
		return err
	}
	return paths.WithLock(ctx, func(ctx context.Context) error {
		value, err := manifest.Load(paths.Manifest())
		if err != nil {
			return err
		}
		lock, err := lockfile.Load(paths.Lockfile())
		if err != nil {
			return err
		}
		resolved, err := manifest.Resolve(value)
		if err != nil {
			return err
		}
		if err := lockfile.ValidateCurrent(resolved, lock); err != nil {
			return err
		}
		downloads, err := paths.OpenDownloads()
		if err != nil {
			return err
		}
		defer func() { _ = downloads.Close() }()
		var results []output.Result
		var invalid []string
		for _, resolvedAsset := range resolved {
			locked := lock.Files[resolvedAsset.Name]
			valid, err := asset.VerifyLocal(downloads, locked.ResolvedFile, locked.Digest, locked.Size)
			if err != nil {
				return err
			}
			if valid && !command.Force {
				results = append(results, output.Result{Name: resolvedAsset.Name, Status: "verified", File: locked.ResolvedFile, Digest: locked.Digest, Size: locked.Size})
				continue
			}
			if command.Offline {
				invalid = append(invalid, resolvedAsset.Name)
				continue
			}
			download, err := command.runtime.Downloader.Download(ctx, downloads, assetRequest(resolvedAsset), locked.Digest)
			if err != nil {
				return err
			}
			if err := download.Commit(); err != nil {
				return project.NewFilesystemError(errors.Join(err, download.Discard()))
			}
			results = append(results, output.Result{Name: resolvedAsset.Name, Status: "downloaded", File: locked.ResolvedFile, Digest: locked.Digest, Size: locked.Size})
		}
		if len(invalid) > 0 {
			return project.NewIntegrityError(fmt.Errorf("offline verification failed for %s", quoteAssetNames(invalid)))
		}
		return command.runtime.Output.Success("pull", paths, results, nil)
	})
}

// quoteAssetNames keeps aggregate human errors unambiguous for opaque names.
func quoteAssetNames(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = strconv.Quote(value)
	}
	return strings.Join(quoted, ", ")
}
