package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/lockfile"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/output"
)

type pullCommand struct {
	runtime *runtime
	Offline bool      `flag:"offline" help:"verify local downloads without network access"`
	Force   bool      `flag:"force" help:"re-download every locked artifact"`
	Jobs    jobsValue `flag:"jobs,j" help:"maximum downloads to run at once"`
}

// pendingPull is one locked asset that pull has decided to download, together
// with the place its outcome belongs in the ordered report.
type pendingPull struct {
	index    int
	request  asset.Request
	source   output.Download
	expected string
	reported bool
}

func (*pullCommand) Description() string { return "Reproduce verified files from dac.lock" }
func (command *pullCommand) Validate() error {
	if err := command.runtime.Output.ValidateOptions(); err != nil {
		return err
	}
	if command.Offline && command.Force {
		return ErrOfflineForceConflict
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
		// Deciding what to download is a question about local state alone, so it
		// is answered for every asset before the first request. That keeps the
		// batch, and the progress line describing it, honest about how much work
		// is left, and leaves the transfers as the only concurrent step.
		assets := resolved.All()
		results := make([]output.Result, 0, len(assets))
		var pending []pendingPull
		var items []output.Download
		var invalid []string
		for _, resolvedAsset := range assets {
			locked := lock.Files[resolvedAsset.Name]
			result := output.Result{Name: resolvedAsset.Name, Status: "verified", File: locked.ResolvedFile, Digest: locked.Digest, Size: locked.Size}
			valid, err := asset.VerifyLocal(downloads.Root(), locked.ResolvedFile, locked.Digest, locked.Size)
			if err != nil {
				return err
			}
			if valid && !command.Force {
				results = append(results, result)
				continue
			}
			if command.Offline {
				invalid = append(invalid, resolvedAsset.Name)
				continue
			}
			result.Status = "downloaded"
			source := output.Download{File: locked.ResolvedFile, URL: locked.ResolvedURL}
			pending = append(pending, pendingPull{
				index:    len(results),
				request:  assetRequest(resolvedAsset),
				source:   source,
				expected: locked.Digest,
			})
			items = append(items, source)
			results = append(results, result)
		}
		if len(invalid) > 0 {
			return fault.NewIntegrityError(fmt.Errorf("%w for %s", ErrOfflineVerification, quoteAssetNames(invalid)))
		}
		err = command.runtime.Output.WithDownloads(ctx, items, func(ctx context.Context, group *output.DownloadGroup) error {
			return downloadEach(ctx, command.Jobs.limit(), pending, func(ctx context.Context, item *pendingPull) error {
				reported, err := group.Run(ctx, item.source, func(ctx context.Context) error {
					return command.install(ctx, downloads.Root(), item)
				})
				item.reported = reported
				return err
			})
		})
		if err != nil {
			return err
		}
		for _, item := range pending {
			results[item.index].Reported = item.reported
		}
		return command.runtime.Output.Success("pull", paths.Root, results, nil)
	})
}

// install replaces one managed file with bytes that match the accepted digest.
// Every asset stages beside its own destination, so installing one download
// never observes another in progress.
func (command *pullCommand) install(ctx context.Context, downloads *os.Root, item *pendingPull) error {
	download, err := command.runtime.Downloader.Download(ctx, downloads, item.request, item.expected)
	if err != nil {
		return err
	}
	if err := download.Commit(); err != nil {
		return fault.NewFilesystemError(errors.Join(err, download.Discard()))
	}
	return nil
}

// quoteAssetNames keeps aggregate human errors unambiguous for opaque names.
func quoteAssetNames(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = strconv.Quote(value)
	}
	return strings.Join(quoted, ", ")
}
