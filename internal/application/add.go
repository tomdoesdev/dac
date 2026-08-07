package application

import (
	"context"
	"strings"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/kit/fs/util/filename"
)

// AddOptions defines one manifest addition.
type AddOptions struct {
	Coordinate coord.Coordinate
	URL        string
	Integrity  string
	// Filename is the project name that overrides origin names during later resolutions.
	Filename string
	Force    bool
	// Pin records the digest the asset resolves to as its integrity value, so that every later command holds the publisher to the bytes this add saw.
	Pin bool
	// MaxSize bounds the one download performed by Pin.
	MaxSize int64
	// Offline explicitly refuses Pin's required request. Non-pin additions are always local.
	Offline bool
}

// AddResult reports one manifest addition.
type AddResult struct {
	Asset
	// Siblings names the other versions of this asset the project already had.
	Siblings []string `json:"siblings"`
	// SharedSources names the sibling versions served from the same URL as this one.
	SharedSources []string `json:"sharedSources"`
	// Locked remains for result compatibility. Add never locks assets; pull owns all lock-file changes.
	Locked []string `json:"locked"`
}

// Add writes one asset to the manifest without changing the lock file.
// Pin is the sole networked form: it downloads the new asset to record its digest
// as manifest intent, while still leaving lock-file reconciliation to pull.
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
		URL:       options.URL,
		Integrity: integrity,
		Filename:  name,
	}
	updated.Assets[options.Coordinate] = asset
	if err := updated.Validate(); err != nil {
		return AddResult{}, fault.Wrap("invalid_arguments", "The asset is invalid.", err)
	}
	// Both lists describe the project as it stands after this addition, so they are read from the updated manifest with the new coordinate itself dropped.
	siblings := coord.Versions(others(coord.InGroup(updated.Assets, options.Coordinate.Group()), options.Coordinate))
	shared := sharedSources(updated, options.Coordinate, options.URL)
	if !options.Pin {
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

	// Pin resolves only the new coordinate. Reconciliation is deliberately not used:
	// it represents a complete lock-file view and could resolve unrelated stale entries.
	service.Reporter.Plan([]string{options.Coordinate.String()})
	resolved, status, err := service.resolve(ctx, options.Coordinate, asset, project.LockAsset{}, reconcileOptions{
		maxSize: options.MaxSize,
		mode:    resolveChanged,
	})
	if err != nil {
		if ctx.Err() == nil {
			service.Reporter.Fail(options.Coordinate.String(), err)
		}
		return AddResult{}, err
	}
	// Pin records the digest from the bytes that this command received. The resolved
	// metadata stays in the cache/catalog until pull chooses what belongs in the lock.
	asset.Integrity = resolved.Digest
	updated.Assets[options.Coordinate] = asset
	if err := project.Write(service.ManifestPath, updated); err != nil {
		return AddResult{}, fault.Wrap("project_write_failed", "DAC could not write the manifest file.", err)
	}
	view, err := service.assetView(options.Coordinate, asset, resolved, status)
	if err != nil {
		return AddResult{}, err
	}
	return AddResult{Asset: view, Siblings: siblings, SharedSources: shared, Locked: []string{}}, nil
}

// others drops one coordinate from a list while preserving the original order.
func others(names []coord.Coordinate, name coord.Coordinate) []coord.Coordinate {
	rest := make([]coord.Coordinate, 0, len(names))
	for _, value := range names {
		if value != name {
			rest = append(rest, value)
		}
	}
	return rest
}
