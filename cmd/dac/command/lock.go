package command

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strconv"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/lockfile"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/output"
	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/kit/fs/atomic"
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
		transaction := newLockTransaction()
		defer transaction.discard()
		for _, resolvedAsset := range resolved {
			if !selected[resolvedAsset.Name] {
				continue
			}
			var download *asset.StagedDownload
			err = command.runtime.Output.WithDownloadProgress(ctx, resolvedAsset.Name, resolvedAsset.ResolvedFile, resolvedAsset.ResolvedURL, func(ctx context.Context) error {
				download, err = command.runtime.Downloader.Download(ctx, downloads, assetRequest(resolvedAsset), resolvedAsset.Pin)
				return err
			})
			if err != nil {
				return err
			}
			transaction.downloads[resolvedAsset.Name] = download
			next.Files[resolvedAsset.Name] = lockfile.Asset{
				ResolvedURL: resolvedAsset.ResolvedURL, ResolvedFile: resolvedAsset.ResolvedFile,
				ConfigurationDigest: lockfile.ConfigurationDigest(resolvedAsset), Digest: download.Digest, Size: download.Size,
			}
		}
		// The manifest is authoritative even for targeted locks; entries for
		// deleted assets must not survive in accepted state.
		validNames := make(map[string]bool, len(resolved))
		for _, resolvedAsset := range resolved {
			validNames[resolvedAsset.Name] = true
		}
		for name := range next.Files {
			if !validNames[name] {
				delete(next.Files, name)
			}
		}
		transaction.lock, err = lockfile.Stage(paths.Lockfile(), next)
		if err != nil {
			return err
		}
		warnings, err := transaction.commit(resolved, initial)
		if err != nil {
			return err
		}
		if loadWarning != "" {
			warnings = append(warnings, loadWarning)
		}
		if hasLock {
			warnings = append(warnings, cleanupObsoleteDownloads(downloads, current, next)...)
		}
		results := make([]output.Result, 0, len(selected))
		for _, resolvedAsset := range resolved {
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
		if _, err := os.Lstat(paths.Lockfile()); err == nil {
			return lockfile.Lockfile{}, false, "", fault.NewConfigurationError(ErrLockAlreadyExists)
		} else if errors.Is(err, fs.ErrNotExist) {
			return lockfile.Lockfile{}, false, "", nil
		} else {
			return lockfile.Lockfile{}, false, "", fault.NewFilesystemError(err)
		}
	}
	current, exists, err := lockfile.LoadOptional(paths.Lockfile())
	if err == nil {
		return current, exists, "", nil
	}
	if !command.All {
		return lockfile.Lockfile{}, false, "", err
	}
	if _, statErr := os.Stat(paths.Lockfile()); errors.Is(statErr, fs.ErrNotExist) {
		return lockfile.Lockfile{}, false, "", nil
	}
	return lockfile.Lockfile{}, false, "existing dac.lock could not be read; obsolete downloads were not pruned", nil
}

// cleanupObsoleteDownloads removes only destinations proven to have belonged
// to the previous valid lock. Untracked entries remain available to status.
func cleanupObsoleteDownloads(downloads *os.Root, previous, next lockfile.Lockfile) []string {
	retained := make(map[string]bool, len(next.Files))
	for _, file := range next.Files {
		retained[file.ResolvedFile] = true
	}
	var warnings []string
	removed := false
	seen := make(map[string]bool, len(previous.Files))
	for _, file := range previous.Files {
		name := file.ResolvedFile
		if retained[name] || seen[name] {
			continue
		}
		seen[name] = true
		info, err := downloads.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			warnings = append(warnings, "could not inspect obsolete download "+strconv.Quote(name)+": "+err.Error())
			continue
		}
		if info.IsDir() {
			warnings = append(warnings, "obsolete download is a directory and was not removed: "+strconv.Quote(name))
			continue
		}
		if err := downloads.Remove(name); err != nil {
			warnings = append(warnings, "could not remove obsolete download "+strconv.Quote(name)+": "+err.Error())
			continue
		}
		removed = true
	}
	if removed {
		directory, err := downloads.Open(".")
		if err == nil {
			err = errors.Join(directory.Sync(), directory.Close())
		}
		if err != nil {
			warnings = append(warnings, "could not sync downloads after removing obsolete files: "+err.Error())
		}
	}
	return warnings
}

func selectedNames(names []string, values []manifest.ResolvedAsset) (map[string]bool, error) {
	result := make(map[string]bool, len(values))
	known := make(map[string]bool, len(values))
	for _, value := range values {
		known[value.Name] = true
	}
	if len(names) == 0 {
		for name := range known {
			result[name] = true
		}
		return result, nil
	}
	for _, name := range names {
		if !known[name] {
			return nil, fault.NewConfigurationError(ErrAssetNotFound, fault.WithAsset(name))
		}
		if result[name] {
			return nil, fault.NewConfigurationError(ErrAssetSelectedMultipleTimes, fault.WithAsset(name))
		}
		result[name] = true
	}
	return result, nil
}

// lockTransaction owns temporary files and reversible commits so the handler's
// business logic cannot accidentally skip rollback or cleanup on one branch.
type lockTransaction struct {
	downloads map[string]*asset.StagedDownload
	lock      *atomic.File
}

func newLockTransaction() *lockTransaction {
	return &lockTransaction{downloads: make(map[string]*asset.StagedDownload)}
}

func (transaction *lockTransaction) discard() {
	for _, download := range transaction.downloads {
		_ = download.Discard()
	}
	if transaction.lock != nil {
		_ = transaction.lock.Discard()
	}
}

// commit installs downloaded assets before atomically installing their lock
// metadata, rolling the whole operation back when any commit fails.
func (transaction *lockTransaction) commit(order []manifest.ResolvedAsset, createOnly bool) ([]string, error) {
	commits := make([]*atomic.Commit, 0, len(transaction.downloads)+1)
	for _, resolvedAsset := range order {
		download := transaction.downloads[resolvedAsset.Name]
		if download == nil {
			continue
		}
		commit, err := download.CommitReversible()
		if commit != nil {
			commits = append(commits, commit)
		}
		if err != nil {
			return nil, rollback(commits, fault.NewFilesystemError(err))
		}
	}
	var commit *atomic.Commit
	var err error
	if createOnly {
		// A no-replace commit closes the race between the initial existence check
		// and installing accepted metadata after all downloads have completed.
		commit, err = transaction.lock.CommitNoReplace()
	} else {
		commit, err = transaction.lock.CommitReversible()
	}
	if commit != nil {
		commits = append(commits, commit)
	}
	if err != nil {
		if createOnly && errors.Is(err, fs.ErrExist) {
			return nil, rollback(commits, fault.NewConfigurationError(ErrLockAlreadyExists))
		}
		return nil, rollback(commits, fault.NewFilesystemError(err))
	}
	warnings := make([]string, 0)
	for _, commit := range commits {
		if err := commit.Complete(); err != nil {
			warnings = append(warnings, "could not remove transaction recovery file: "+err.Error())
		}
	}
	return warnings, nil
}

func rollback(commits []*atomic.Commit, original error) error {
	cleanup := make([]error, 0, len(commits)+1)
	cleanup = append(cleanup, original)
	for index := len(commits) - 1; index >= 0; index-- {
		cleanup = append(cleanup, commits[index].Rollback())
	}
	return errors.Join(cleanup...)
}
