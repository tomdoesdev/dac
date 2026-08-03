package application

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/tom/dac/internal/coord"
	"github.com/tom/dac/internal/digest"
	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/project"
)

// PullResult reports a pull operation.
type PullResult struct {
	ManifestDigest string `json:"manifestDigest"`
	// Locked names the assets this pull resolved to bring the lock file into
	// agreement with the manifest. It is empty without --update-lock, which is
	// the only way a pull writes that file at all.
	Locked []string `json:"locked"`
	AssetSummary
}

// Pull installs every missing locked asset, updating the lock file first when
// the command asked it to.
func (service *Service) Pull(ctx context.Context, options NetworkOptions) (PullResult, error) {
	defer service.Reporter.Wait()
	manifest, lock, reconciled, err := service.pullProject(ctx, options)
	if err != nil {
		return PullResult{}, err
	}
	names := manifest.Coordinates()
	assets, err := parallel(ctx, options.Concurrency, names, func(ctx context.Context, name coord.Coordinate) (Asset, error) {
		source, locked := manifest.Assets[name], lock.Assets[name]
		// A reconcile that resolved this asset has already installed its bytes
		// and already reported it. Pulling it again would print a second bar for
		// a transfer that finished a moment ago.
		if status, found := reconciled.resolved[name]; found {
			return service.assetView(name, source, locked, installStatus(status))
		}
		value, err := service.pull(ctx, name, source, locked, options)
		if err != nil && ctx.Err() == nil {
			service.Reporter.Fail(name.String(), err)
		}
		return value, err
	})
	if err != nil {
		return PullResult{}, err
	}
	return PullResult{
		ManifestDigest: lock.ManifestDigest,
		Locked:         reconciled.names(names),
		AssetSummary:   collect(assets),
	}, nil
}

// pullProject returns the manifest and lock one pull installs from.
//
// Without --update-lock it reads them and refuses a lock that does not describe
// the manifest, which is what makes a pull in a deployment job reproduce the
// project as committed rather than as the manifest now reads. With it, the pull
// resolves whatever the lock is missing and writes the result first.
func (service *Service) pullProject(ctx context.Context, options NetworkOptions) (project.Manifest, project.Lock, reconciliation, error) {
	if !options.UpdateLock {
		manifest, lock, err := service.readProject()
		return manifest, lock, reconciliation{}, err
	}
	manifest, err := service.readManifest()
	if err != nil {
		return project.Manifest{}, project.Lock{}, reconciliation{}, err
	}
	old, err := service.readLockIfPresent()
	if err != nil {
		return project.Manifest{}, project.Lock{}, reconciliation{}, err
	}
	reconciled, err := service.reconcile(ctx, manifest, old, options)
	if err != nil {
		return project.Manifest{}, project.Lock{}, reconciliation{}, err
	}
	lock, err := newLock(manifest, reconciled.assets)
	if err != nil {
		return project.Manifest{}, project.Lock{}, reconciliation{}, err
	}
	if _, err := service.writeLock(lock); err != nil {
		return project.Manifest{}, project.Lock{}, reconciliation{}, err
	}
	return manifest, lock, reconciled, nil
}

// installStatus reports what a reconciled asset cost this command. Resolving one
// stored its bytes, so this pull downloaded them whatever the reconcile called
// it. The rest keep the status they were settled with: an asset the origin
// answered not-modified is a different outcome from one the cache answered
// alone, and only the first proves the origin was asked.
func installStatus(status string) string {
	if status == statusResolved {
		return "downloaded"
	}
	return status
}

func (service *Service) pull(ctx context.Context, coordinate coord.Coordinate, source project.Asset, locked project.LockAsset, options NetworkOptions) (Asset, error) {
	name := coordinate.String()
	object := Object{Digest: locked.Digest, Size: locked.Size}
	// The view already answers whether the cache holds these bytes, so a hit
	// costs one lookup rather than one to decide and another to report.
	view, err := service.assetView(coordinate, source, locked, "cached")
	if err != nil {
		return Asset{}, err
	}
	service.Reporter.Start(name, locked.Size)
	if view.Cached {
		service.Reporter.Done(name, "cached")
		return view, nil
	}
	// A corrupt object is downloaded again rather than reported. The install
	// renames the good bytes over the bad ones, so the pull that noticed the
	// damage is also the command that repairs it.
	status := "downloaded"
	if view.Corrupt {
		status = "repaired"
	}
	err = service.Store.WithLock(ctx, locked.Digest, func() error {
		valid, err := service.usable(object)
		if err != nil {
			return err
		}
		if valid {
			status = "cached"
			service.Reporter.Done(name, status)
			return nil
		}
		if options.DistDir != "" {
			found, err := service.installDist(ctx, options.DistDir, locked)
			if err != nil {
				return err
			}
			if found {
				status = "distdir"
				service.Reporter.Done(name, status)
				return nil
			}
		}
		if options.Offline {
			// A corrupt object offline is a different problem from a missing
			// one: there is damage to report and no way to repair it here.
			if view.Corrupt {
				return &fault.Error{
					Code:    "cache_object_corrupt",
					Message: "The cache object does not match its digest, and offline mode cannot replace it.",
					Details: map[string]any{"expectedDigest": locked.Digest},
				}
			}
			return fault.New("offline_cache_miss", "Offline mode could not find the required cache object.")
		}
		response, err := service.fetch(ctx, project.Asset{URL: source.URL, AllowInsecureHTTP: source.AllowInsecureHTTP}, "")
		if err != nil {
			return err
		}
		defer func() { _ = response.Body.Close() }()
		if response.NotModified {
			return fault.New("network_error", "The server returned an invalid not-modified response.")
		}
		if response.Length >= 0 && response.Length != locked.Size {
			mismatch := &ContentError{ExpectedDigest: locked.Digest, ExpectedSize: locked.Size, ActualSize: response.Length}
			return &fault.Error{
				Code:    "content_size_mismatch",
				Message: "The downloaded asset size does not match the lock file.",
				Details: mismatch.Details(),
				Cause:   mismatch,
			}
		}
		reader := &progressReader{name: name, reader: response.Body, reporter: service.Reporter}
		if _, err := service.Store.Put(ctx, reader, PutExact(object)); err != nil {
			return contentError(err)
		}
		service.Reporter.Done(name, status)
		return nil
	})
	if err != nil {
		return Asset{}, err
	}
	return service.assetView(coordinate, source, locked, status)
}

// installDist installs one locked asset from a distribution directory. The
// caller holds the digest lock.
//
// A distribution directory holds objects named by digest. This is the only
// name that a consumer can check. DAC used to accept the final URL element.
// That name was a guess, so DAC could not report a mismatch correctly.
func (service *Service) installDist(ctx context.Context, directory string, locked project.LockAsset) (bool, error) {
	hexValue, err := digest.Hex(locked.Digest)
	if err != nil {
		return false, fault.Wrap("lock_invalid", "The lock file has an invalid digest.", err)
	}
	file, err := os.Open(filepath.Join(directory, hexValue))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fault.Wrap("distdir_read_failed", "DAC could not read the distribution directory.", err)
	}
	_, err = service.Store.Put(ctx, file, PutExact(Object{Digest: locked.Digest, Size: locked.Size}))
	_ = file.Close()
	if err != nil {
		return false, contentError(err)
	}
	return true, nil
}
