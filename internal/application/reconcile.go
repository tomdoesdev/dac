package application

// This file holds the operation that brings a lock file into agreement with a manifest.

import (
	"bytes"
	"context"
	"errors"
	"os"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/filename"
	"github.com/tomdoesdev/dac/internal/project"
)

// The status one reconcile records for each asset it settled.
const (
	statusLocked      = "locked"
	statusResolved    = "resolved"
	statusNotModified = "not_modified"
)

// reconciliation is the asset set one reconcile produced, and what each entry cost to produce.
type reconciliation struct {
	// assets is the complete set for a new lock: the entries this reconcile resolved and the entries it carried through, together.
	assets map[coord.Coordinate]project.LockAsset
	// resolved records the status of each entry this reconcile had to reach the origin for.
	resolved map[coord.Coordinate]string
}

type resolveMode uint8

const (
	resolveChanged resolveMode = iota
	resolveRefresh
	resolveObserve
)

// reconcileOptions holds the internal choices for one reconciliation.
type reconcileOptions struct {
	concurrency int
	maxSize     int64
	mode        resolveMode
}

// names returns the assets this reconcile resolved, in manifest order.
func (result reconciliation) names(order []coord.Coordinate) []string {
	names := make([]string, 0, len(result.resolved))
	for _, name := range order {
		if _, found := result.resolved[name]; found {
			names = append(names, name.String())
		}
	}
	return names
}

// reconcile brings a lock into agreement with a manifest by resolving only the assets the lock no longer describes.
// Reconcile reuses agreeing lock entries to avoid origin requests.
// It writes nothing.
func (service *Service) reconcile(ctx context.Context, manifest project.Manifest, old project.Lock, options reconcileOptions) (reconciliation, error) {
	names := manifest.Coordinates()
	service.Reporter.Plan(coord.Strings(names))
	settled, err := parallel(ctx, options.concurrency, names, func(ctx context.Context, name coord.Coordinate) (resolvedAsset, error) {
		source := manifest.Assets[name]
		locked, exists := old.Assets[name]
		if options.mode == resolveChanged && project.Agrees(source, locked, exists) {
			// A pinned asset neither sends nor records an ETag.
			if source.Integrity != "" {
				locked.ETag = ""
			}
			// DAC settles names without downloads because names do not affect object bytes.
			// A declared name is applied whenever the manifest carries one, so renaming an asset is a manifest edit and a lock, with no request.
			if declared := filename.Clean(source.Filename); declared != "" {
				locked.Filename = declared
			} else if locked.Filename == "" {
				locked.Filename = filename.FromURL(source.URL)
			}
			return resolvedAsset{lock: locked, status: statusLocked}, nil
		}
		value, status, err := service.resolve(ctx, name, source, locked, options)
		if err != nil {
			if ctx.Err() == nil {
				service.Reporter.Fail(name.String(), err)
			}
			return resolvedAsset{}, err
		}
		return resolvedAsset{lock: value, status: status}, nil
	})
	if err != nil {
		return reconciliation{}, err
	}
	result := reconciliation{
		assets:   make(map[coord.Coordinate]project.LockAsset, len(names)),
		resolved: map[coord.Coordinate]string{},
	}
	for index, name := range names {
		result.assets[name] = settled[index].lock
		if settled[index].status != statusLocked {
			result.resolved[name] = settled[index].status
		}
	}
	return result, nil
}

type resolvedAsset struct {
	lock   project.LockAsset
	status string
}

// readLockIfPresent reads the lock a reconcile starts from.
func (service *Service) readLockIfPresent() (project.Lock, error) {
	lock, _, err := project.ReadLockIfPresent(service.LockPath)
	if err != nil {
		return project.Lock{}, fault.Wrap("lock_invalid", "The lock file is invalid.", err)
	}
	return lock, nil
}

// writeLock writes a lock file and reports whether it had to.
func (service *Service) writeLock(lock project.Lock) (bool, error) {
	data, err := project.Marshal(lock)
	if err != nil {
		return false, fault.Wrap("lock_write_failed", "DAC could not encode the lock file.", err)
	}
	existing, err := os.ReadFile(service.LockPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fault.Wrap("lock_write_failed", "DAC could not read the lock file.", err)
	}
	if bytes.Equal(existing, data) {
		return false, nil
	}
	if err := project.WriteBytes(service.LockPath, data); err != nil {
		return false, fault.Wrap("lock_write_failed", "DAC could not write the lock file.", err)
	}
	return true, nil
}

// newLock builds a lock for a manifest and its resolved assets.
func newLock(manifest project.Manifest, assets map[coord.Coordinate]project.LockAsset) (project.Lock, error) {
	lock, err := project.NewLock(manifest, assets)
	if err != nil {
		return project.Lock{}, fault.Wrap("lock_invalid", "DAC created an invalid lock file.", err)
	}
	return lock, nil
}
