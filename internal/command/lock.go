package command

import (
	"context"
	"errors"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/lockfile"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/output"
	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/kit/fs/atomic"
)

type lockCommand struct {
	runtime *runtime
	Names   []string `arg:"names"`
}

func (*lockCommand) Description() string { return "Accept current upstream bytes into dac.lock" }
func (command *lockCommand) Validate() error {
	return command.runtime.Output.ValidateOptions()
}

func (command *lockCommand) Run(ctx context.Context) error {
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
		downloads, err := paths.OpenDownloads()
		if err != nil {
			return err
		}
		defer func() { _ = downloads.Close() }()
		current, hasLock, err := lockfile.LoadOptional(paths.Lockfile())
		if err != nil {
			return err
		}
		selected, err := selectedNames(command.Names, resolved)
		if err != nil {
			return err
		}
		if len(command.Names) > 0 {
			if !hasLock && len(selected) != len(resolved) {
				return &project.Error{Kind: "configuration", Hint: "run `dac lock`", Err: errors.New("targeted lock requires a complete existing dac.lock")}
			}
			if hasLock {
				if err := lockfile.ValidateRetained(resolved, current, selected); err != nil {
					return err
				}
			}
		}

		next := lockfile.Lockfile{Version: project.Version, Files: map[string]lockfile.Asset{}}
		if hasLock {
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
			download, err := command.runtime.Downloader.Download(ctx, downloads, assetRequest(resolvedAsset), resolvedAsset.Pin)
			if err != nil {
				return err
			}
			transaction.downloads[resolvedAsset.Name] = download
			next.Files[resolvedAsset.Name] = lockfile.Asset{ResolvedURL: resolvedAsset.ResolvedURL, ResolvedFile: resolvedAsset.ResolvedFile, Digest: download.Digest, Size: download.Size}
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
		warnings, err := transaction.commit(resolved)
		if err != nil {
			return err
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
			return nil, &project.Error{Kind: "configuration", Asset: name, Err: errors.New("asset does not exist")}
		}
		if result[name] {
			return nil, &project.Error{Kind: "configuration", Asset: name, Err: errors.New("asset selected more than once")}
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

func (transaction *lockTransaction) commit(order []manifest.ResolvedAsset) ([]string, error) {
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
			return nil, rollback(commits, project.NewError("filesystem", err))
		}
	}
	commit, err := transaction.lock.CommitReversible()
	if commit != nil {
		commits = append(commits, commit)
	}
	if err != nil {
		return nil, rollback(commits, project.NewError("filesystem", err))
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
