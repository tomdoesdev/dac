package application

// This file holds the lock file command: the operation that reads the manifest, resolves what the lock file does not already describe, and writes it.
// It is the only operation whose product is the lock file itself.

import (
	"context"
)

// LockResult reports one lock file update.
type LockResult struct {
	ManifestDigest string `json:"manifestDigest"`
	// Locked names the assets this command had to reach an origin for, in manifest order.
	Locked []string `json:"locked"`
	// Changed reports whether the file on disk moved.
	Changed bool `json:"changed"`
	AssetSummary
}

// LockOptions controls one lock operation.
type LockOptions struct {
	Concurrency int
	MaxSize     int64
	Refresh     bool
}

// Lock brings the lock file into agreement with the manifest.
// By default, Lock resolves only entries that do not agree with the manifest.
// Resolving an asset means downloading it, so this warms the cache for the assets it settles.
func (service *Service) Lock(ctx context.Context, options LockOptions) (LockResult, error) {
	defer service.Reporter.Wait()
	manifest, err := service.readManifest()
	if err != nil {
		return LockResult{}, err
	}
	// A lock file that does not exist yet is the state a first lock is there to leave behind, so it is read if present rather than required.
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
		// The status says what this asset cost: statusLocked for an entry carried through untouched, and otherwise whatever reaching the origin produced.
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
