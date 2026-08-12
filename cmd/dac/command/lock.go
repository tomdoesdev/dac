package command

import (
	"context"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/lockfile"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/output"
	"github.com/tomdoesdev/dac/internal/project"
)

type lockCommand struct {
	runtime *runtime
	Names   []string `arg:"names"`
	All     bool     `flag:"all" help:"replace dac.lock from every manifest asset"`
}

func (*lockCommand) Description() string { return "Accept current upstream bytes into dac.lock" }
func (command *lockCommand) Validate() error {
	if err := command.runtime.Output.ValidateOptions(); err != nil {
		return err
	}
	if command.All && len(command.Names) > 0 {
		return ErrAllLockConflict
	}
	return nil
}

func (command *lockCommand) Run(ctx context.Context) error {
	paths, err := command.runtime.project()
	if err != nil {
		return err
	}
	return paths.WithLock(ctx, func(ctx context.Context) error {
		initial := !command.All && len(command.Names) == 0
		current, hasLock, loadWarning, err := command.loadCurrent(paths)
		if err != nil {
			return err
		}
		value, err := manifest.Load(paths.Manifest())
		if err != nil {
			return err
		}
		resolved, err := manifest.Resolve(value)
		if err != nil {
			return err
		}
		downloads, err := paths.OpenDownloads()
		if err != nil {
			return err
		}
		defer func() { _ = downloads.Close() }()
		selected, err := selectedNames(command.Names, resolved)
		if err != nil {
			return err
		}
		if !command.All && !initial {
			if !hasLock {
				return fault.NewConfigurationError(ErrTargetedLockNeedsExisting, fault.WithRecovery(fault.Recovery{Command: "lock", Flags: []string{"--all"}}))
			}
			if err := lockfile.ValidateRetained(resolved, current, selected); err != nil {
				return err
			}
		}

		next := lockfile.Lockfile{Version: lockfile.Version, Files: map[string]lockfile.Asset{}}
		if hasLock && !command.All {
			for name, file := range current.Files {
				next.Files[name] = file
			}
		}
		transaction := lockfile.NewTransaction()
		defer transaction.Discard()
		order := make([]string, 0, resolved.Len())
		for _, resolvedAsset := range resolved.All() {
			order = append(order, resolvedAsset.Name)
			if !selected[resolvedAsset.Name] {
				continue
			}
			var download *asset.StagedDownload
			err = command.runtime.Output.WithDownloadProgress(ctx, resolvedAsset.Name, resolvedAsset.ResolvedFile, resolvedAsset.ResolvedURL, func(ctx context.Context) error {
				download, err = command.runtime.Downloader.Download(ctx, downloads.Root(), assetRequest(resolvedAsset), resolvedAsset.Pin)
				return err
			})
			if err != nil {
				return err
			}
			transaction.Add(resolvedAsset.Name, download)
			next.Files[resolvedAsset.Name] = lockfile.Asset{
				ResolvedURL: resolvedAsset.ResolvedURL, ResolvedFile: resolvedAsset.ResolvedFile,
				ConfigurationDigest: lockfile.ConfigurationDigest(resolvedAsset), Digest: download.Digest, Size: download.Size,
			}
		}
		// The manifest is authoritative even for targeted locks; entries for
		// deleted assets must not survive in accepted state.
		for name := range next.Files {
			if !resolved.Has(name) {
				delete(next.Files, name)
			}
		}
		if err := transaction.Stage(paths.Lockfile(), next); err != nil {
			return err
		}
		warnings, err := transaction.Commit(order, initial)
		if err != nil {
			return err
		}
		if loadWarning != "" {
			warnings = append(warnings, loadWarning)
		}
		if hasLock {
			warnings = append(warnings, downloads.Prune(current.ResolvedFiles(), next.ResolvedFiles())...)
		}
		results := make([]output.Result, 0, len(selected))
		for _, resolvedAsset := range resolved.All() {
			if file, ok := next.Files[resolvedAsset.Name]; ok && selected[resolvedAsset.Name] {
				results = append(results, output.Result{Name: resolvedAsset.Name, Status: "locked", File: file.ResolvedFile, Digest: file.Digest, Size: file.Size})
			}
		}
		return command.runtime.Output.Success("lock", paths, results, warnings)
	})
}

// loadCurrent makes a bare lock create-only and permits --all to recover from
// unreadable or unsupported lock metadata.
func (command *lockCommand) loadCurrent(paths project.Paths) (lockfile.Lockfile, bool, string, error) {
	if !command.All && len(command.Names) == 0 {
		// A bare lock creates dac.lock and never replaces accepted state. The
		// probe does not follow symlinks, so a link standing in for the lock
		// still counts as present; CommitNoReplace closes the remaining race.
		exists, err := lockfile.Exists(paths.Lockfile())
		if err != nil {
			return lockfile.Lockfile{}, false, "", err
		}
		if exists {
			return lockfile.Lockfile{}, false, "", fault.NewConfigurationError(lockfile.ErrAlreadyExists)
		}
		return lockfile.Lockfile{}, false, "", nil
	}
	current, exists, err := lockfile.LoadOptional(paths.Lockfile())
	if err == nil {
		return current, exists, "", nil
	}
	if !command.All {
		return lockfile.Lockfile{}, false, "", err
	}
	// LoadOptional reports absence as a nil error, so reaching here with --all
	// means the file is present but unusable. Replace it wholesale and warn
	// that its download bookkeeping could not be read.
	return lockfile.Lockfile{}, false, "existing dac.lock could not be read; obsolete downloads were not pruned", nil
}

// selectedNames resolves a command's asset arguments against the manifest. No
// arguments selects everything the manifest declares.
func selectedNames(names []string, resolved manifest.Resolution) (map[string]bool, error) {
	result := make(map[string]bool, resolved.Len())
	if len(names) == 0 {
		for _, value := range resolved.All() {
			result[value.Name] = true
		}
		return result, nil
	}
	for _, name := range names {
		if !resolved.Has(name) {
			return nil, fault.NewConfigurationError(ErrAssetNotFound, fault.WithAsset(name))
		}
		if result[name] {
			return nil, fault.NewConfigurationError(ErrAssetSelectedMultipleTimes, fault.WithAsset(name))
		}
		result[name] = true
	}
	return result, nil
}
