package application

import (
	"context"
	"maps"

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
	MaxSize           int64
	Offline           bool
}

// Add writes one asset to the manifest and resolves it unless offline mode is active.
func (service *Service) Add(ctx context.Context, options AddOptions) (Asset, error) {
	defer service.Reporter.Wait()
	var manifest project.Manifest
	var lock project.Lock
	var err error
	if options.Offline {
		manifest, err = service.readManifest()
	} else {
		manifest, lock, err = service.readProject()
	}
	if err != nil {
		return Asset{}, err
	}
	if _, exists := manifest.Assets[options.Name]; exists && !options.Force {
		return Asset{}, fault.New("asset_exists", "The manifest already has this asset. Use --force to replace it.")
	}
	integrity := ""
	if options.Integrity != "" {
		integrity, err = digest.Canonical(options.Integrity)
		if err != nil {
			return Asset{}, fault.Wrap("invalid_arguments", "The integrity value is invalid.", err)
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
		return Asset{}, fault.Wrap("invalid_arguments", "The asset is invalid.", err)
	}
	if options.Offline {
		if err := project.Write(service.ManifestPath, updated); err != nil {
			return Asset{}, fault.Wrap("project_write_failed", "DAC could not write the manifest file.", err)
		}
		return Asset{Name: options.Name, Version: asset.Version, URL: asset.URL, Integrity: asset.Integrity, Status: "unlocked"}, nil
	}

	resolved, status, err := service.resolve(ctx, options.Name, asset, lock.Assets[options.Name], NetworkOptions{MaxSize: options.MaxSize})
	if err != nil {
		service.Reporter.Fail(options.Name, err)
		return Asset{}, withAsset(err, options.Name)
	}
	assets := maps.Clone(lock.Assets)
	assets[options.Name] = resolved
	newLock, err := project.NewLock(updated, assets)
	if err != nil {
		return Asset{}, fault.Wrap("lock_invalid", "DAC created an invalid lock file.", err)
	}
	if err := project.WritePair(service.ManifestPath, service.LockPath, updated, newLock); err != nil {
		return Asset{}, fault.Wrap("project_write_failed", "DAC could not write the project files.", err)
	}
	return service.assetView(options.Name, asset, resolved, status)
}
