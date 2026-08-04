package application

import (
	"context"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
)

// PullOptions controls one pull.
type PullOptions struct {
	NetworkOptions
	// Assets narrows the pull to the coordinates these selections name. An
	// empty list is the whole project.
	//
	// A project holds every asset every job built from it needs, and a job
	// needs the ones it needs. Without this, fetching one asset out of twenty
	// means fetching twenty: dac path requires the object to be cached
	// already, so a plain pull was the only way to put it there.
	Assets []Selection
}

// PullResult reports a pull operation.
type PullResult struct {
	ManifestDigest string `json:"manifestDigest"`
	// ProjectCount is how many assets the project has. It differs from the
	// asset count only when the pull was narrowed, which is what lets a
	// consumer tell "there was one asset" from "one asset was asked for".
	//
	// It was added without an output version bump, for the reason the file name
	// field was: a new field breaks no consumer that does not read it.
	ProjectCount int `json:"projectCount"`
	AssetSummary
}

// Pull installs the missing locked assets, or the ones a selection names.
//
// It writes nothing. A pull refuses a lock file that does not describe the
// manifest rather than settling the difference, which is what makes a pull in a
// deployment job reproduce the project as committed rather than as the manifest
// now reads. Bringing the two files back into agreement is dac lock.
//
// Narrowing a pull narrows what it fetches and nothing else. The whole project
// is still read and the lock file still has to describe the manifest, because
// the question "is this project what was committed" is not one that a command
// fetching half of it gets to skip.
func (service *Service) Pull(ctx context.Context, options PullOptions) (PullResult, error) {
	defer service.Reporter.Wait()
	manifest, lock, err := service.readProject()
	if err != nil {
		return PullResult{}, err
	}
	names, err := chosen(manifest, options.Assets)
	if err != nil {
		return PullResult{}, err
	}
	assets, err := parallel(ctx, options.Concurrency, names, func(ctx context.Context, name coord.Coordinate) (Asset, error) {
		value, err := service.pull(ctx, name, manifest.Assets[name], lock.Assets[name], options.NetworkOptions)
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
		ProjectCount:   len(manifest.Assets),
		AssetSummary:   collect(assets),
	}, nil
}

// chosen returns the coordinates a list of selections covers, in manifest order
// and without repeats. An empty list covers the whole project.
//
// The order is the manifest's rather than the command line's, so that a result
// reads the same whether or not it was narrowed, and two selections that overlap
// -- an asset named once whole and once by its group -- fetch it once.
func chosen(manifest project.Manifest, selections []Selection) ([]coord.Coordinate, error) {
	if len(selections) == 0 {
		return manifest.Coordinates(), nil
	}
	wanted := make(map[coord.Coordinate]struct{}, len(selections))
	for _, selection := range selections {
		names, err := selected(manifest, selection)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			wanted[name] = struct{}{}
		}
	}
	names := make([]coord.Coordinate, 0, len(wanted))
	for _, name := range manifest.Coordinates() {
		if _, found := wanted[name]; found {
			names = append(names, name)
		}
	}
	return names, nil
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
