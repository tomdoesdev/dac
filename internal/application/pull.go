package application

import (
	"context"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
)

// PullOptions controls one pull.
type PullOptions struct {
	Concurrency int
	MaxSize     int64
	Offline     bool
	// Assets narrows the pull to the coordinates these selections name.
	// A project holds every asset every job built from it needs, and a job needs the ones it needs.
	Assets []Selection
}

// PullResult reports a pull operation.
type PullResult struct {
	ManifestDigest string `json:"manifestDigest"`
	// ProjectCount is how many assets the project has.
	// It was added without an output version bump, for the reason the file name field was: a new field breaks no consumer that does not read it.
	ProjectCount int `json:"projectCount"`
	AssetSummary
}

// Pull installs the missing locked assets, or the ones a selection names.
// It writes nothing.
// Narrowing a pull narrows what it fetches and nothing else.
func (service *Service) Pull(ctx context.Context, options PullOptions) (PullResult, error) {
	defer service.Reporter.Wait()
	manifest, lock, err := service.readProject()
	if err != nil {
		return PullResult{}, err
	}
	names, err := chosen(manifest.Assets, options.Assets)
	if err != nil {
		return PullResult{}, err
	}
	service.Reporter.Plan(coord.Strings(names))
	assets, err := parallel(ctx, options.Concurrency, names, func(ctx context.Context, name coord.Coordinate) (Asset, error) {
		value, err := service.pull(ctx, name, manifest.Assets[name], lock.Assets[name], options)
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

func (service *Service) pull(ctx context.Context, coordinate coord.Coordinate, source project.Asset, locked project.LockAsset, options PullOptions) (Asset, error) {
	name := coordinate.String()
	object := Object{Digest: locked.Digest, Size: locked.Size}
	// The view already answers whether the cache holds these bytes, so a hit costs one lookup rather than one to decide and another to report.
	view, err := service.assetView(coordinate, source, locked, "cached")
	if err != nil {
		return Asset{}, err
	}
	service.Reporter.Start(name, locked.Size)
	if view.Cached {
		service.Reporter.Done(name, "cached")
		return view, nil
	}
	// A corrupt object is downloaded again rather than reported.
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
			// A corrupt object offline is a different problem from a missing one: there is damage to report and no way to repair it here.
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
		reader := &transferReader{name: name, reader: response.Body, reporter: service.Reporter}
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
