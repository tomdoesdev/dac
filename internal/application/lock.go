package application

// This file holds the lock file command: the operation that reads the manifest,
// resolves what the lock file does not already describe, and writes it.
//
// It is the only operation whose product is the lock file itself. Add and remove
// maintain that file because they are already changing the project, and pull
// refuses a lock file it disagrees with rather than settling the difference. So
// the decision to rewrite what a project's assets resolve to is made here, by a
// command an operator ran on purpose.

import (
	"context"
)

// LockResult reports one lock file update.
type LockResult struct {
	ManifestDigest string `json:"manifestDigest"`
	// Locked names the assets this command had to reach an origin for, in
	// manifest order. An entry the lock already described costs no request and is
	// absent, which is what makes this the list an operator reviews against the
	// diff they are about to commit.
	Locked []string `json:"locked"`
	// Changed reports whether the file on disk moved. A lock file can be rewritten
	// without resolving anything -- a backfilled file name, an ETag dropped
	// because the manifest pinned the asset -- and a lock file can be left exactly
	// as it was found. Neither is visible from Locked alone.
	Changed bool `json:"changed"`
	AssetSummary
}

// LockOptions controls one lock operation.
type LockOptions struct {
	Concurrency int
	MaxSize     int64
	Refresh     bool
	AllowRebind bool
}

// Lock brings the lock file into agreement with the manifest.
//
// By default it resolves only the assets the lock file does not describe, which
// is what makes locking a project that is already mostly locked cost one request
// per genuine change. Refresh resolves every asset against its origin instead.
//
// Resolving an asset means downloading it, so this warms the cache for the
// assets it settles. It installs nothing else: a project's missing objects are
// pull's business, and the two commands are separate because writing down what a
// project uses and fetching what it uses are separate decisions.
func (service *Service) Lock(ctx context.Context, options LockOptions) (LockResult, error) {
	defer service.Reporter.Wait()
	manifest, err := service.readManifest()
	if err != nil {
		return LockResult{}, err
	}
	// A lock file that does not exist yet is the state a first lock is there to
	// leave behind, so it is read if present rather than required.
	old, err := service.readLockIfPresent()
	if err != nil {
		return LockResult{}, err
	}
	mode := resolveChanged
	if options.Refresh {
		mode = resolveRefresh
	}
	reconciled, err := service.reconcile(ctx, manifest, old, reconcileOptions{
		concurrency: options.Concurrency,
		maxSize:     options.MaxSize,
		mode:        mode,
		allowRebind: options.AllowRebind,
	})
	if err != nil {
		return LockResult{}, err
	}
	lock, err := newLock(manifest, reconciled.assets)
	if err != nil {
		return LockResult{}, err
	}
	changed, err := service.writeLock(lock)
	if err != nil {
		return LockResult{}, err
	}
	names := manifest.Coordinates()
	assets := make([]Asset, 0, len(names))
	for _, name := range names {
		// The status says what this asset cost: statusLocked for an entry carried
		// through untouched, and otherwise whatever reaching the origin produced.
		// An asset the origin answered not-modified is a different outcome from one
		// that was downloaded, and only the lock file's own record distinguishes
		// them afterwards.
		status := statusLocked
		if resolved, found := reconciled.resolved[name]; found {
			status = resolved
		}
		view, err := service.assetView(name, manifest.Assets[name], reconciled.assets[name], status)
		if err != nil {
			return LockResult{}, err
		}
		assets = append(assets, view)
	}
	return LockResult{
		ManifestDigest: lock.ManifestDigest,
		Locked:         reconciled.names(names),
		Changed:        changed,
		AssetSummary:   collect(assets),
	}, nil
}
