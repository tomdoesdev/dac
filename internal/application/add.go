package application

import (
	"context"

	"github.com/tom/dac/internal/coord"
	"github.com/tom/dac/internal/digest"
	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/project"
)

// AddOptions defines one manifest addition.
type AddOptions struct {
	Coordinate        coord.Coordinate
	URL               string
	Integrity         string
	AllowInsecureHTTP bool
	Force             bool
	// Pin records the digest the asset resolves to as its integrity value, so
	// that every later command holds the publisher to the bytes this add saw.
	Pin     bool
	MaxSize int64
	Offline bool
	// AllowRebind accepts a new source for a coordinate the lock file already
	// binds to different bytes. Replacing the entry is what --force permits;
	// changing what the version means is a second decision.
	AllowRebind bool
}

// AddResult reports one manifest addition.
type AddResult struct {
	Asset
	// Siblings names the other versions of this asset the project already had.
	// Adding a version no longer retires the one before it, so this is how an
	// operator sees that a project now carries two rather than discovering it in
	// the manifest diff.
	Siblings []string `json:"siblings"`
	// SharedSources names the sibling versions served from the same URL as this
	// one. Only one set of bytes can be at a URL, so at most one of those
	// versions can ever be restored to a cold cache.
	SharedSources []string `json:"sharedSources"`
	// Locked names the other assets this add had to resolve, which is every
	// asset a hand edit left the lock file no longer describing. An add is
	// allowed to settle them, but not to settle them silently.
	Locked []string `json:"locked"`
}

// Add writes one asset to the manifest and resolves it unless offline mode is active.
func (service *Service) Add(ctx context.Context, options AddOptions) (AddResult, error) {
	defer service.Reporter.Wait()
	if options.Pin && options.Integrity != "" {
		return AddResult{}, fault.New("invalid_arguments", "Use --pin or --integrity, not both.")
	}
	if options.Pin && options.Offline {
		return AddResult{}, fault.New("invalid_arguments", "Offline mode cannot pin an asset, because it resolves no bytes to pin to.")
	}
	manifest, err := service.readManifest()
	if err != nil {
		return AddResult{}, err
	}
	// An add reads whatever lock file is there and settles the difference
	// itself, so adding an asset to a project that has never been locked works
	// the same as adding one to a project that has. The manifest is the file it
	// will not create: that is the declaration that a project exists here, and
	// inventing one would turn a mistyped --manifest path into a new project
	// instead of an error.
	var lock project.Lock
	if !options.Offline {
		lock, err = service.readLockIfPresent()
		if err != nil {
			return AddResult{}, err
		}
	}
	// A coordinate is the whole identity of an asset, so this asks only about
	// the exact version. Adding another version of an asset the project already
	// has is an addition rather than a replacement, and it needs no --force:
	// nothing that referred to the old coordinate stops working.
	if _, exists := manifest.Assets[options.Coordinate]; exists && !options.Force {
		return AddResult{}, fault.New("asset_exists", "The manifest already has this asset version. Use --force to replace its source.")
	}
	integrity := ""
	if options.Integrity != "" {
		integrity, err = digest.Canonical(options.Integrity)
		if err != nil {
			return AddResult{}, fault.Wrap("invalid_arguments", "The integrity value is invalid.", err)
		}
	}
	updated := manifest.Clone()
	asset := project.Asset{
		URL:               options.URL,
		Integrity:         integrity,
		AllowInsecureHTTP: options.AllowInsecureHTTP,
	}
	updated.Assets[options.Coordinate] = asset
	if err := updated.Validate(); err != nil {
		return AddResult{}, fault.Wrap("invalid_arguments", "The asset is invalid.", err)
	}
	// Both lists describe the project as it stands after this addition, so they
	// are read from the updated manifest with the new coordinate itself dropped.
	siblings := coord.Versions(others(coord.InGroup(updated.Assets, options.Coordinate.Group()), options.Coordinate))
	shared := sharedSources(updated, options.Coordinate, options.URL)
	if options.Offline {
		if err := project.Write(service.ManifestPath, updated); err != nil {
			return AddResult{}, fault.Wrap("project_write_failed", "DAC could not write the manifest file.", err)
		}
		view := Asset{
			Coordinate: options.Coordinate.String(),
			Namespace:  options.Coordinate.Namespace,
			Name:       options.Coordinate.Name,
			Version:    options.Coordinate.Version,
			URL:        asset.URL,
			Integrity:  asset.Integrity,
			Status:     "unlocked",
		}
		return AddResult{Asset: view, Siblings: siblings, SharedSources: shared, Locked: []string{}}, nil
	}

	// Reconciling the updated manifest resolves the new asset, which no lock
	// file can describe yet, and any asset a hand edit left behind, in one pass.
	// Adding one asset to a project whose manifest someone edited would
	// otherwise write a lock file that every later command rejects as stale.
	reconciled, err := service.reconcile(ctx, updated, lock, NetworkOptions{
		MaxSize:     options.MaxSize,
		AllowRebind: options.AllowRebind,
	})
	if err != nil {
		return AddResult{}, err
	}
	resolved := reconciled.assets[options.Coordinate]
	if options.Pin {
		// Pinning after resolution is what makes it useful: the digest comes
		// from the bytes this command actually received, which is the value an
		// operator would otherwise copy out of the summary and paste back in.
		asset.Integrity = resolved.Digest
		updated.Assets[options.Coordinate] = asset
		// A pinned asset neither sends nor records an ETag, so drop the one this
		// resolution collected. Leaving it would make the next pull rewrite the
		// file for no reason and report drift that never happened.
		resolved.ETag = ""
		reconciled.assets[options.Coordinate] = resolved
	}
	updatedLock, err := newLock(updated, reconciled.assets)
	if err != nil {
		return AddResult{}, err
	}
	if err := project.WritePair(service.ManifestPath, service.LockPath, updated, updatedLock); err != nil {
		return AddResult{}, fault.Wrap("project_write_failed", "DAC could not write the project files.", err)
	}
	view, err := service.assetView(options.Coordinate, asset, resolved, reconciled.resolved[options.Coordinate])
	if err != nil {
		return AddResult{}, err
	}
	locked := reconciled.names(others(updated.Coordinates(), options.Coordinate))
	return AddResult{Asset: view, Siblings: siblings, SharedSources: shared, Locked: locked}, nil
}

// others drops one coordinate from a list, leaving the assets a command
// settled or already held on the way to the one it was asked about.
func others(names []coord.Coordinate, name coord.Coordinate) []coord.Coordinate {
	rest := make([]coord.Coordinate, 0, len(names))
	for _, value := range names {
		if value != name {
			rest = append(rest, value)
		}
	}
	return rest
}
