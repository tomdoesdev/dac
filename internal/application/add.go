package application

import (
	"context"

	"github.com/tom/dac/internal/digest"
	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/project"
)

// AddOptions defines one manifest addition.
type AddOptions struct {
	Name              string
	Version           string
	URL               string
	Integrity         string
	AllowInsecureHTTP bool
	Force             bool
	// Pin records the digest the asset resolves to as its integrity value, so
	// that every later command holds the publisher to the bytes this add saw.
	Pin     bool
	MaxSize int64
	Offline bool
}

// AddResult reports one manifest addition.
type AddResult struct {
	Asset
	// Replaced names the coordinate this add retired. DAC keeps one version of
	// each asset name, so replacing an asset at a different version quietly
	// invalidates every reference to the old one: saying so is the difference
	// between an edit and a surprise.
	Replaced string `json:"replaced,omitempty"`
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
	replaced := ""
	if previous, exists := manifest.Assets[options.Name]; exists {
		if !options.Force {
			return AddResult{}, fault.New("asset_exists", "The manifest already has this asset. Use --force to replace it.")
		}
		if previous.Version != options.Version {
			replaced = options.Name + "@" + previous.Version
		}
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
		Version:           options.Version,
		URL:               options.URL,
		Integrity:         integrity,
		AllowInsecureHTTP: options.AllowInsecureHTTP,
	}
	updated.Assets[options.Name] = asset
	if err := updated.Validate(); err != nil {
		return AddResult{}, fault.Wrap("invalid_arguments", "The asset is invalid.", err)
	}
	if options.Offline {
		if err := project.Write(service.ManifestPath, updated); err != nil {
			return AddResult{}, fault.Wrap("project_write_failed", "DAC could not write the manifest file.", err)
		}
		view := Asset{Name: options.Name, Version: asset.Version, URL: asset.URL, Integrity: asset.Integrity, Status: "unlocked"}
		return AddResult{Asset: view, Replaced: replaced, Locked: []string{}}, nil
	}

	// Reconciling the updated manifest resolves the new asset, which no lock
	// file can describe yet, and any asset a hand edit left behind, in one pass.
	// Adding one asset to a project whose manifest someone edited would
	// otherwise write a lock file that every later command rejects as stale.
	reconciled, err := service.reconcile(ctx, updated, lock, NetworkOptions{MaxSize: options.MaxSize})
	if err != nil {
		return AddResult{}, err
	}
	resolved := reconciled.assets[options.Name]
	if options.Pin {
		// Pinning after resolution is what makes it useful: the digest comes
		// from the bytes this command actually received, which is the value an
		// operator would otherwise copy out of the summary and paste back in.
		asset.Integrity = resolved.Digest
		updated.Assets[options.Name] = asset
		// A pinned asset neither sends nor records an ETag, so drop the one this
		// resolution collected. Leaving it would make the next pull rewrite the
		// file for no reason and report drift that never happened.
		resolved.ETag = ""
		reconciled.assets[options.Name] = resolved
	}
	updatedLock, err := newLock(updated, reconciled.assets)
	if err != nil {
		return AddResult{}, err
	}
	if err := project.WritePair(service.ManifestPath, service.LockPath, updated, updatedLock); err != nil {
		return AddResult{}, fault.Wrap("project_write_failed", "DAC could not write the project files.", err)
	}
	view, err := service.assetView(options.Name, asset, resolved, reconciled.resolved[options.Name])
	if err != nil {
		return AddResult{}, err
	}
	locked := reconciled.names(updated.Names())
	return AddResult{Asset: view, Replaced: replaced, Locked: others(locked, options.Name)}, nil
}

// others drops the named asset from a list, leaving the assets a command
// settled on the way to the one it was asked about.
func others(names []string, name string) []string {
	rest := make([]string, 0, len(names))
	for _, value := range names {
		if value != name {
			rest = append(rest, value)
		}
	}
	return rest
}
