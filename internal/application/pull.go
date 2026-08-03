package application

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/tom/dac/internal/digest"
	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/project"
)

// PullResult reports a pull operation.
type PullResult struct {
	ManifestDigest string `json:"manifestDigest"`
	AssetSummary
}

// Pull installs every missing locked asset.
func (service *Service) Pull(ctx context.Context, options NetworkOptions) (PullResult, error) {
	defer service.Reporter.Wait()
	manifest, lock, err := service.readProject()
	if err != nil {
		return PullResult{}, err
	}
	names := manifest.Names()
	assets, err := parallel(ctx, options.Concurrency, names, func(ctx context.Context, name string) (Asset, error) {
		value, err := service.pull(ctx, name, manifest.Assets[name], lock.Assets[name], options)
		if err != nil && ctx.Err() == nil {
			service.Reporter.Fail(name, err)
		}
		return value, err
	})
	if err != nil {
		return PullResult{}, err
	}
	return PullResult{ManifestDigest: lock.ManifestDigest, AssetSummary: collect(assets)}, nil
}

func (service *Service) pull(ctx context.Context, name string, source project.Asset, locked project.LockAsset, options NetworkOptions) (Asset, error) {
	object := Object{Digest: locked.Digest, Size: locked.Size}
	// The view already answers whether the cache holds these bytes, so a hit
	// costs one lookup rather than one to decide and another to report.
	view, err := service.assetView(name, source, locked, "cached")
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
	return service.assetView(name, source, locked, status)
}

// installDist installs one locked asset from a distribution directory. The
// caller holds the digest lock.
//
// A distribution directory holds objects named by digest, which is what dac
// export writes and the only naming a consumer can check. DAC used to also
// accept a file named after the last element of the asset URL, and that
// leniency cost more than it gave: the name was a guess, so a mismatch had to
// be skipped rather than reported, and a bundle with one wrong file failed as
// "not found" instead of "wrong bytes".
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
