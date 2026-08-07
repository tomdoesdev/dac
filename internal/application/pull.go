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
	// Refresh resolves the selected assets against their origins even when the lock
	// already agrees. Plain pull still reconciles manifest changes before installing.
	Refresh bool
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
	// Locked names the assets this pull resolved from an origin or a pinned cache object to settle the lock file, in manifest order.
	Locked []string `json:"locked"`
	// Changed reports whether the lock file on disk moved.
	Changed bool `json:"changed"`
	AssetSummary
}

// Pull installs the locked assets, or the ones a selection names.
//
// A project with a missing or stale lock gets reconciled before installation. An
// already-current lock is left byte-for-byte alone unless Refresh explicitly asks
// to observe origins again. This makes pull the only command that changes lock state.
//
// Narrowing a pull narrows what it installs, and which origins a refresh reaches.
func (service *Service) Pull(ctx context.Context, options PullOptions) (PullResult, error) {
	defer service.Reporter.Wait()
	if options.Offline && options.Refresh {
		return PullResult{}, fault.New("invalid_arguments",
			"Offline mode cannot refresh the lock file, because it resolves no bytes to write one from.")
	}
	manifest, err := service.readManifest()
	if err != nil {
		return PullResult{}, err
	}
	lock, present, err := service.readLockIfPresent()
	if err != nil {
		return PullResult{}, err
	}
	names, err := chosen(manifest.Assets, options.Assets)
	if err != nil {
		return PullResult{}, err
	}
	written := relocked{locked: []string{}}
	current := false
	if present {
		current = project.CheckLock(manifest, lock) == nil
	}
	switch {
	case current && !options.Refresh:
		// The common install path consumes the committed lock without rewriting it.
	case !present && options.Offline:
		// Refresh is already refused above, so this is a project with no lock file at all.
		return PullResult{}, fault.New("lock_missing",
			"The lock file does not exist, and offline mode resolves nothing to write one from. Run dac pull.")
	default:
		written, err = service.relock(ctx, manifest, lock, names, options)
		if err != nil {
			return PullResult{}, err
		}
		lock = written.lock
	}
	// The lock is settled here whichever branch above settled it, including the one that agreed
	// with the manifest and so never reconciled.
	service.note(lock.Assets)
	service.Reporter.Plan(coord.Strings(names))
	assets, err := parallel(ctx, options.Concurrency, names, func(ctx context.Context, name coord.Coordinate) (Asset, error) {
		value, err := service.pull(ctx, name, manifest.Assets[name], lock.Assets[name], options)
		if err != nil && ctx.Err() == nil {
			service.Reporter.Fail(name.String(), err)
		}
		// An asset the lock phase resolved sits in the cache because this run put it there.
		// The status says what the run did, rather than what the install behind it then found.
		if status, settled := written.resolved[name]; settled && err == nil {
			value.Status = status
		}
		return value, err
	})
	if err != nil {
		return PullResult{}, err
	}
	return PullResult{
		ManifestDigest: lock.ManifestDigest,
		ProjectCount:   len(manifest.Assets),
		Locked:         written.locked,
		Changed:        written.changed,
		AssetSummary:   collect(assets),
	}, nil
}

// relocked is the lock file one pull wrote, and what writing it cost.
type relocked struct {
	lock    project.Lock
	locked  []string
	changed bool
	// resolved is the status of each entry the lock phase reached an origin for, by coordinate.
	resolved map[coord.Coordinate]string
}

// relock settles the lock file a pull is about to install from.
//
// The named assets resolve against their origins when this is a refresh. Every other entry
// still reconciles, because a lock file describes the whole manifest: leaving one behind
// would write a file that CheckLock rejects on the next run.
func (service *Service) relock(ctx context.Context, manifest project.Manifest, old project.Lock, names []coord.Coordinate, options PullOptions) (relocked, error) {
	refresh := map[coord.Coordinate]struct{}{}
	if options.Refresh {
		for _, name := range names {
			refresh[name] = struct{}{}
		}
	}
	reconciled, err := service.reconcile(ctx, manifest, old, reconcileOptions{
		concurrency: options.Concurrency,
		maxSize:     options.MaxSize,
		mode:        resolveChanged,
		offline:     options.Offline,
		refresh:     refresh,
	})
	if err != nil {
		return relocked{}, err
	}
	lock, err := newLock(manifest, reconciled.assets)
	if err != nil {
		return relocked{}, err
	}
	changed, err := service.writeLock(lock)
	if err != nil {
		return relocked{}, err
	}
	return relocked{
		lock:     lock,
		locked:   reconciled.names(manifest.Coordinates()),
		changed:  changed,
		resolved: reconciled.resolved,
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
		response, err := service.fetch(ctx, project.Asset{URL: source.URL}, project.LockAsset{})
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
		// Recorded as it lands, so a pull interrupted part way through still leaves a cache DAC
		// can account for.
		service.noteAsset(coordinate, locked)
		service.Reporter.Done(name, status)
		return nil
	})
	if err != nil {
		return Asset{}, err
	}
	return service.assetView(coordinate, source, locked, status)
}
