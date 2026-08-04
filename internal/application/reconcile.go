package application

// This file holds the operation that brings a lock file into agreement with a
// manifest. It is what dac lock runs, and what add runs over the manifest it is
// about to write, so that both settle a hand-edited project the same way.

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

// The status one reconcile records for each asset it settled. The first marks
// an entry the lock already described, which cost nothing; the rest come from
// resolve and say what reaching the origin cost.
const (
	statusLocked      = "locked"
	statusResolved    = "resolved"
	statusNotModified = "not_modified"
)

// reconciliation is the asset set one reconcile produced, and what each entry
// cost to produce.
type reconciliation struct {
	// assets is the complete set for a new lock: the entries this reconcile
	// resolved and the entries it carried through, together.
	assets map[coord.Coordinate]project.LockAsset
	// resolved records the status of each entry this reconcile had to reach the
	// origin for. An entry the lock already described is absent, which is how a
	// caller tells the two apart without comparing lock files.
	resolved map[coord.Coordinate]string
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

// reconcile brings a lock into agreement with a manifest by resolving only the
// assets the lock no longer describes.
//
// An entry the lock still describes is carried through without a request, so
// locking a project that is already mostly locked costs one request per genuine
// change rather than one per asset. Resolving everything on every run is what
// --refresh asks for, and it is a different question: not "what does this
// project still not describe" but "does every origin still serve what we wrote
// down".
//
// It writes nothing. A caller that changes the manifest in the same command has
// to write both files together or neither, so the decision to write belongs to
// the caller rather than to this.
func (service *Service) reconcile(ctx context.Context, manifest project.Manifest, old project.Lock, options NetworkOptions) (reconciliation, error) {
	names := manifest.Coordinates()
	settled, err := parallel(ctx, options.Concurrency, names, func(ctx context.Context, name coord.Coordinate) (resolvedAsset, error) {
		source := manifest.Assets[name]
		locked, exists := old.Assets[name]
		if !options.Refresh && project.Agrees(source, locked, exists) {
			// A pinned asset neither sends nor records an ETag. One the lock
			// carries from before the manifest pinned it is inert, because
			// nothing would ever send it, but leaving it there would have the
			// lock file contradict what the manifest says about the asset.
			if source.Integrity != "" {
				locked.ETag = ""
			}
			// A file name is settled here as well as in resolve, because an entry
			// that still agrees with its manifest is never resolved again and a
			// name is not something to make anybody re-download an asset for.
			//
			// A declared name is applied whenever the manifest carries one, so
			// renaming an asset is a manifest edit and a lock, with no request.
			// Otherwise an entry from a lock written before file names were
			// recorded has none: the URL spells one without asking anybody, so
			// fill it in here rather than leave a project that only gains names
			// for the assets that happen to change.
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
		// A coordinate the lock already binds may not come back naming other
		// bytes. The reporter is not told when it does: the transfer it was
		// following finished, and what failed is the claim about the bytes.
		if exists && !options.AllowRebind {
			if err := checkRebind(name, locked, value); err != nil {
				return resolvedAsset{}, err
			}
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
	// Both version rules run over the finished set rather than per asset,
	// because both compare one asset against another and neither can be decided
	// while the other resolutions are still in flight.
	if err := project.CheckVersions(result.assets); err != nil {
		return reconciliation{}, versionFault(err)
	}
	if !options.AllowRebind {
		if err := checkRetired(result.assets, old); err != nil {
			return reconciliation{}, err
		}
	}
	return result, nil
}

type resolvedAsset struct {
	lock   project.LockAsset
	status string
}

// readLockIfPresent reads the lock a reconcile starts from. A lock that does not
// exist yet is the state a first pull is there to leave behind, so it is not an
// error; a lock that exists and cannot be read is.
func (service *Service) readLockIfPresent() (project.Lock, error) {
	lock, _, err := project.ReadLockIfPresent(service.LockPath)
	if err != nil {
		return project.Lock{}, lockFault("lock_invalid", "The lock file is invalid.", err)
	}
	return lock, nil
}

// writeLock writes a lock file and reports whether it had to. Rewriting
// identical bytes would move the file's modification time for nothing, which
// anything watching the project directory would read as a change.
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
		return project.Lock{}, lockFault("lock_invalid", "DAC created an invalid lock file.", err)
	}
	return lock, nil
}
