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
	"github.com/tomdoesdev/dac/internal/project"
)

type pullCommand struct {
	runtime        *runtime
	Names          []string  `arg:"names"`
	Offline        bool      `flag:"offline" help:"verify local downloads without network access"`
	Force          bool      `flag:"force" help:"re-download every selected locked artifact"`
	UpdateLockfile bool      `flag:"update-lockfile" help:"accept upstream bytes for selected stale assets"`
	Jobs           jobsValue `flag:"jobs,j" help:"maximum downloads to run at once"`
}

// pendingPull is one selected asset pull has decided to download, together
// with the place its outcome belongs in the ordered report.
type pendingPull struct {
	index    int
	resolved manifest.ResolvedAsset
	request  asset.Request
	source   output.Download
	expected string
	stale    bool
	download *asset.StagedDownload
	reported bool
}

// pullSelection records whether the caller named a scope explicitly. An
// explicit list remains scoped even when it happens to contain every asset,
// because only a bare pull owns whole-lock orphan cleanup.
type pullSelection struct {
	assets   []manifest.ResolvedAsset
	names    map[string]bool
	complete bool
}

func (*pullCommand) Description() string { return "Reproduce files and optionally update dac.lock" }
func (command *pullCommand) Validate() error {
	if err := command.runtime.Output.ValidateOptions(); err != nil {
		return err
	}
	if command.Offline && command.UpdateLockfile {
		return ErrOfflineUpdateConflict
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
		resolved, err := manifest.Resolve(value)
		if err != nil {
			return err
		}
		selection, err := selectPullAssets(command.Names, resolved)
		if err != nil {
			return err
		}
		locked, hasLock, err := loadPullLock(paths)
		if err != nil {
			return err
		}
		if !hasLock {
			if !command.UpdateLockfile {
				return fault.NewConfigurationError(lockfile.ErrNotFound, fault.WithRecovery(lockfile.UpdateRecovery(selection.recoveryNames()...)))
			}
			locked = lockfile.Lockfile{Version: lockfile.Version, Files: map[string]lockfile.Asset{}}
		}
		evaluation, err := lockfile.Evaluate(resolved, locked)
		if err != nil {
			return err
		}
		if !command.UpdateLockfile {
			if err := lockfile.ValidateSelected(evaluation, selection.names, selection.complete); err != nil {
				return err
			}
		}
		downloads, err := paths.OpenDownloads()
		if err != nil {
			return err
		}
		defer func() { _ = downloads.Close() }()
		if command.UpdateLockfile && pullNeedsLockUpdate(evaluation, selection, hasLock) {
			return command.pullUpdating(ctx, paths, downloads, selection, evaluation, locked, hasLock)
		}
		return command.pullCurrent(ctx, paths, downloads, selection.assets, locked)
	})
}

// pullCurrent reproduces only already accepted entries. Downloads commit
// independently, preserving ordinary pull's established partial-progress
// behavior when no accepted state is changing.
func (command *pullCommand) pullCurrent(ctx context.Context, paths project.Paths, downloads *project.Downloads, assets []manifest.ResolvedAsset, locked lockfile.Lockfile) error {
	// Deciding what to download is a question about local state alone, so it
	// is answered for every asset before the first request. That keeps the
	// batch, and the progress line describing it, honest about how much work
	// is left, and leaves the transfers as the only concurrent step.
	results := make([]output.Result, 0, len(assets))
	var pending []pendingPull
	var items []output.Download
	var invalid []string
	for _, resolvedAsset := range assets {
		lockedAsset := locked.Files[resolvedAsset.Name]
		result := output.Result{Name: resolvedAsset.Name, Status: "verified", File: lockedAsset.ResolvedFile, Digest: lockedAsset.Digest, Size: lockedAsset.Size}
		valid, err := asset.VerifyLocal(downloads.Root(), lockedAsset.ResolvedFile, lockedAsset.Digest, lockedAsset.Size)
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
		source := output.Download{File: lockedAsset.ResolvedFile, URL: lockedAsset.ResolvedURL}
		pending = append(pending, pendingPull{
			index:    len(results),
			request:  assetRequest(resolvedAsset),
			source:   source,
			expected: lockedAsset.Digest,
		})
		items = append(items, source)
		results = append(results, result)
	}
	if len(invalid) > 0 {
		return fault.NewIntegrityError(fmt.Errorf("%w for %s", ErrOfflineVerification, quoteAssetNames(invalid)))
	}
	err := command.runtime.Output.WithDownloads(ctx, items, func(ctx context.Context, group *output.DownloadGroup) error {
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
}

// pullUpdating stages every selected transfer before changing accepted state.
// This keeps newly accepted digests and the exact bytes they describe in one
// reversible transaction, including forced repairs of current entries.
func (command *pullCommand) pullUpdating(ctx context.Context, paths project.Paths, downloads *project.Downloads, selection pullSelection, evaluation lockfile.Evaluation, current lockfile.Lockfile, hasLock bool) error {
	next := lockfile.Lockfile{Version: lockfile.Version, Files: make(map[string]lockfile.Asset, len(current.Files))}
	for name, file := range current.Files {
		next.Files[name] = file
	}
	if selection.complete {
		for _, name := range evaluation.Orphans {
			delete(next.Files, name)
		}
	}
	states := make(map[string]lockfile.EntryState, len(evaluation.Entries))
	for _, entry := range evaluation.Entries {
		states[entry.Resolved.Name] = entry
	}

	results := make([]output.Result, 0, len(selection.assets))
	pending := make([]pendingPull, 0, len(selection.assets))
	items := make([]output.Download, 0, len(selection.assets))
	for _, resolvedAsset := range selection.assets {
		entry := states[resolvedAsset.Name]
		if entry.State == lockfile.StateCurrent {
			result := output.Result{Name: resolvedAsset.Name, Status: "verified", File: entry.Locked.ResolvedFile, Digest: entry.Locked.Digest, Size: entry.Locked.Size}
			valid, err := asset.VerifyLocal(downloads.Root(), entry.Locked.ResolvedFile, entry.Locked.Digest, entry.Locked.Size)
			if err != nil {
				return err
			}
			if valid && !command.Force {
				results = append(results, result)
				continue
			}
			result.Status = "downloaded"
			source := output.Download{File: entry.Locked.ResolvedFile, URL: entry.Locked.ResolvedURL}
			pending = append(pending, pendingPull{
				index: len(results), resolved: resolvedAsset,
				request: assetRequest(resolvedAsset), source: source, expected: entry.Locked.Digest,
			})
			items = append(items, source)
			results = append(results, result)
			continue
		}

		source := output.Download{File: resolvedAsset.ResolvedFile, URL: resolvedAsset.ResolvedURL}
		pending = append(pending, pendingPull{
			index: len(results), resolved: resolvedAsset, request: assetRequest(resolvedAsset),
			source: source, expected: resolvedAsset.Pin, stale: true,
		})
		items = append(items, source)
		results = append(results, output.Result{Name: resolvedAsset.Name, Status: "downloaded", File: resolvedAsset.ResolvedFile})
	}

	transaction := lockfile.NewTransaction()
	defer transaction.Discard()
	downloadErr := command.runtime.Output.WithDownloads(ctx, items, func(ctx context.Context, group *output.DownloadGroup) error {
		return downloadEach(ctx, command.Jobs.limit(), pending, func(ctx context.Context, item *pendingPull) error {
			reported, err := group.Run(ctx, item.source, func(ctx context.Context) error {
				var transferErr error
				item.download, transferErr = command.runtime.Downloader.Download(ctx, downloads.Root(), item.request, item.expected)
				return transferErr
			})
			item.reported = reported
			return err
		})
	})
	for index := range pending {
		item := &pending[index]
		if item.download == nil {
			continue
		}
		transaction.Add(item.resolved.Name, item.download)
		results[item.index].Reported = item.reported
		if !item.stale {
			continue
		}
		accepted := lockfile.Asset{
			ResolvedURL: item.resolved.ResolvedURL, ResolvedFile: item.resolved.ResolvedFile,
			ConfigurationDigest: lockfile.ConfigurationDigest(item.resolved), Digest: item.download.Digest, Size: item.download.Size,
		}
		next.Files[item.resolved.Name] = accepted
		results[item.index].Digest = accepted.Digest
		results[item.index].Size = accepted.Size
	}
	if downloadErr != nil {
		return downloadErr
	}
	if err := transaction.Stage(paths.Lockfile(), next); err != nil {
		return err
	}
	order := make([]string, 0, len(selection.assets))
	for _, item := range selection.assets {
		order = append(order, item.Name)
	}
	warnings, err := transaction.Commit(order, !hasLock)
	if err != nil {
		return err
	}
	if hasLock {
		warnings = append(warnings, downloads.Prune(current.ResolvedFiles(), next.ResolvedFiles())...)
	}
	return command.runtime.Output.Success("pull", paths.Root, results, warnings)
}

// loadPullLock distinguishes absence from invalid existing state without
// treating a dangling symlink as a safe bootstrap target.
func loadPullLock(paths project.Paths) (lockfile.Lockfile, bool, error) {
	exists, err := lockfile.Exists(paths.Lockfile())
	if err != nil || !exists {
		return lockfile.Lockfile{}, exists, err
	}
	value, err := lockfile.Load(paths.Lockfile())
	return value, true, err
}

// selectPullAssets validates opaque asset IDs and returns them in manifest
// order so concurrency never makes result or commit ordering nondeterministic.
func selectPullAssets(names []string, resolved manifest.Resolution) (pullSelection, error) {
	selected := make(map[string]bool, resolved.Len())
	complete := len(names) == 0
	if complete {
		for _, item := range resolved.All() {
			selected[item.Name] = true
		}
	} else {
		for _, name := range names {
			if !resolved.Has(name) {
				return pullSelection{}, fault.NewConfigurationError(ErrAssetNotFound, fault.WithAsset(name))
			}
			if selected[name] {
				return pullSelection{}, fault.NewConfigurationError(ErrAssetSelectedMultipleTimes, fault.WithAsset(name))
			}
			selected[name] = true
		}
	}
	assets := make([]manifest.ResolvedAsset, 0, len(selected))
	for _, item := range resolved.All() {
		if selected[item.Name] {
			assets = append(assets, item)
		}
	}
	return pullSelection{assets: assets, names: selected, complete: complete}, nil
}

// recoveryNames keeps a bare pull's repair bare while carrying an explicit
// scope through to the suggested updating pull.
func (selection pullSelection) recoveryNames() []string {
	if selection.complete {
		return nil
	}
	names := make([]string, 0, len(selection.assets))
	for _, item := range selection.assets {
		names = append(names, item.Name)
	}
	return names
}

// pullNeedsLockUpdate reports whether the explicit acceptance mode has any
// metadata work to do. Current locks are left byte-for-byte untouched.
func pullNeedsLockUpdate(evaluation lockfile.Evaluation, selection pullSelection, hasLock bool) bool {
	if !hasLock || (selection.complete && len(evaluation.Orphans) > 0) {
		return true
	}
	for _, entry := range evaluation.Entries {
		if selection.names[entry.Resolved.Name] && entry.State == lockfile.StateStale {
			return true
		}
	}
	return false
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
