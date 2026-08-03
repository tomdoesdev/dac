package application

// This file holds the operation that brings a lock file into agreement with a
// manifest. It replaced the lock command: resolving an asset installs its bytes
// in the cache, so a command that resolved everything and a command that
// installed everything were the same command wearing two names.

import (
	"bytes"
	"context"
	"errors"
	"os"

	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/project"
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
	assets map[string]project.LockAsset
	// resolved records the status of each entry this reconcile had to reach the
	// origin for. An entry the lock already described is absent, which is how a
	// caller tells the two apart without comparing lock files.
	resolved map[string]string
}

// names returns the assets this reconcile resolved, in manifest order.
func (result reconciliation) names(order []string) []string {
	names := make([]string, 0, len(result.resolved))
	for _, name := range order {
		if _, found := result.resolved[name]; found {
			names = append(names, name)
		}
	}
	return names
}

// reconcile brings a lock into agreement with a manifest by resolving only the
// assets the lock no longer describes.
//
// An entry the lock still describes is carried through without a request. The
// lock command resolved every asset on every run, which for an unpinned asset
// meant a conditional request whether or not anything had changed. Pull
// answering from a warm cache without touching the network at all is the
// property that makes it usable in a deployment job, and it only survives if
// the assets already locked cost nothing to confirm.
//
// It writes nothing. A caller that changes the manifest in the same command has
// to write both files together or neither, so the decision to write belongs to
// the caller rather than to this.
func (service *Service) reconcile(ctx context.Context, manifest project.Manifest, old project.Lock, options NetworkOptions) (reconciliation, error) {
	names := manifest.Names()
	settled, err := parallel(ctx, options.Concurrency, names, func(ctx context.Context, name string) (resolvedAsset, error) {
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
			return resolvedAsset{lock: locked, status: statusLocked}, nil
		}
		value, status, err := service.resolve(ctx, name, source, locked, options)
		if err != nil && ctx.Err() == nil {
			service.Reporter.Fail(name, err)
		}
		return resolvedAsset{lock: value, status: status}, err
	})
	if err != nil {
		return reconciliation{}, err
	}
	result := reconciliation{
		assets:   make(map[string]project.LockAsset, len(names)),
		resolved: map[string]string{},
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

// readLockIfPresent reads the lock a reconcile starts from. A lock that does not
// exist yet is the state a first pull is there to leave behind, so it is not an
// error; a lock that exists and cannot be read is.
func (service *Service) readLockIfPresent() (project.Lock, error) {
	lock, _, err := project.ReadLockIfPresent(service.LockPath)
	if err != nil {
		return project.Lock{}, fault.Wrap("lock_invalid", "The lock file is invalid.", err)
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
func newLock(manifest project.Manifest, assets map[string]project.LockAsset) (project.Lock, error) {
	lock, err := project.NewLock(manifest, assets)
	if err != nil {
		return project.Lock{}, fault.Wrap("lock_invalid", "DAC created an invalid lock file.", err)
	}
	return lock, nil
}
