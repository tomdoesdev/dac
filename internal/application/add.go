package application

import (
	"context"
	"strings"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/filename"
	"github.com/tomdoesdev/dac/internal/project"
)

// AddOptions defines one manifest addition.
type AddOptions struct {
	Coordinate        coord.Coordinate
	URL               string
	Integrity         string
	AllowInsecureHTTP bool
	// Filename is the project name that overrides origin names during later resolutions.
	Filename string
	// Concurrency bounds the assets one add resolves at a time. An add resolves the
	// asset it was given and every entry a hand edit left the lock file no longer
	// describing, and that second set is the same work lock does.
	Concurrency int
	Force       bool
	// Pin records the digest the asset resolves to as its integrity value, so that every later command holds the publisher to the bytes this add saw.
	Pin     bool
	MaxSize int64
	Offline bool
}

// AddResult reports one manifest addition.
type AddResult struct {
	Asset
	// Siblings names the other versions of this asset the project already had.
	Siblings []string `json:"siblings"`
	// SharedSources names the sibling versions served from the same URL as this one.
	SharedSources []string `json:"sharedSources"`
	// Locked names the other assets this add had to resolve, which is every asset a hand edit left the lock file no longer describing.
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
	// Add settles stale manifest entries while it resolves the new asset.
	var lock project.Lock
	if !options.Offline {
		lock, _, err = service.readLockIfPresent()
		if err != nil {
			return AddResult{}, err
		}
	}
	// A coordinate is the whole identity of an asset, so this asks only about the exact version.
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
	// Add reports an invalid requested name instead of hiding it in a manifest error.
	name := filename.Clean(options.Filename)
	if strings.TrimSpace(options.Filename) != "" && name == "" {
		return AddResult{}, fault.New("invalid_arguments",
			"The file name is invalid. Use one path element: no separator, no control byte, no leading dash, and no more than 255 bytes.")
	}
	updated := manifest.Clone()
	asset := project.Asset{
		URL:               options.URL,
		Integrity:         integrity,
		Filename:          name,
		AllowInsecureHTTP: options.AllowInsecureHTTP,
	}
	updated.Assets[options.Coordinate] = asset
	if err := updated.Validate(); err != nil {
		return AddResult{}, fault.Wrap("invalid_arguments", "The asset is invalid.", err)
	}
	// Both lists describe the project as it stands after this addition, so they are read from the updated manifest with the new coordinate itself dropped.
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
			// A declared name is the one thing about an unresolved asset that is already settled, because the manifest is where it was decided.
			Filename: asset.Filename,
			Status:   "unlocked",
		}
		return AddResult{Asset: view, Siblings: siblings, SharedSources: shared, Locked: []string{}}, nil
	}

	// Reconciling the updated manifest resolves the new asset, which no lock file can describe yet, and any asset a hand edit left behind, in one pass.
	reconciled, err := service.reconcile(ctx, updated, lock, reconcileOptions{
		concurrency: options.Concurrency,
		maxSize:     options.MaxSize,
		mode:        resolveChanged,
	})
	if err != nil {
		return AddResult{}, err
	}
	resolved := reconciled.assets[options.Coordinate]
	if options.Pin {
		// Pin records the digest from the bytes that this command received.
		asset.Integrity = resolved.Digest
		updated.Assets[options.Coordinate] = asset
		// A pinned asset neither sends nor records an ETag, so drop the one this resolution collected.
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

// others drops one coordinate from a list, leaving the assets a command settled or already held on the way to the one it was asked about.
func others(names []coord.Coordinate, name coord.Coordinate) []coord.Coordinate {
	rest := make([]coord.Coordinate, 0, len(names))
	for _, value := range names {
		if value != name {
			rest = append(rest, value)
		}
	}
	return rest
}
