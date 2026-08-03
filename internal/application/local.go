package application

// This file holds the commands that resolve nothing: they read or write project
// state, or collect the cache, and never touch the network.

import (
	"context"
	"errors"
	"maps"
	"os"
	"time"

	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/project"
)

// InitResult reports the files created by init.
type InitResult struct {
	ManifestPath string `json:"manifestPath"`
	LockPath     string `json:"lockPath"`
}

// Init creates matching empty project files.
func (service *Service) Init(force bool) (InitResult, error) {
	if !force {
		for _, path := range []string{service.ManifestPath, service.LockPath} {
			if _, err := os.Stat(path); err == nil {
				return InitResult{}, fault.New("manifest_exists", "The project files already exist. Use --force to replace them.")
			} else if !errors.Is(err, os.ErrNotExist) {
				return InitResult{}, fault.Wrap("project_write_failed", "DAC could not check a project path.", err)
			}
		}
	}
	manifest, lock := project.Empty()
	if err := project.WritePair(service.ManifestPath, service.LockPath, manifest, lock); err != nil {
		return InitResult{}, fault.Wrap("project_write_failed", "DAC could not write the project files.", err)
	}
	return InitResult{ManifestPath: service.ManifestPath, LockPath: service.LockPath}, nil
}

// VerifyResult reports matching project files.
type VerifyResult struct {
	ManifestDigest string `json:"manifestDigest"`
	AssetCount     int    `json:"assetCount"`
}

// Verify checks the project files without network or cache access.
func (service *Service) Verify() (VerifyResult, error) {
	manifest, lock, err := service.readProject()
	if err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{ManifestDigest: lock.ManifestDigest, AssetCount: len(manifest.Assets)}, nil
}

// RemoveResult reports the asset removed from a project.
type RemoveResult struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	AssetCount int    `json:"assetCount"`
}

// Remove deletes one exact coordinate without a network request.
func (service *Service) Remove(name, version string) (RemoveResult, error) {
	manifest, lock, err := service.readProject()
	if err != nil {
		return RemoveResult{}, err
	}
	asset, exists := manifest.Assets[name]
	if !exists || asset.Version != version {
		return RemoveResult{}, fault.New("asset_unknown", "The project does not have this asset coordinate.")
	}
	updated := manifest.Clone()
	delete(updated.Assets, name)
	assets := maps.Clone(lock.Assets)
	delete(assets, name)
	newLock, err := project.NewLock(updated, assets)
	if err != nil {
		return RemoveResult{}, fault.Wrap("lock_invalid", "DAC created an invalid lock file.", err)
	}
	if err := project.WritePair(service.ManifestPath, service.LockPath, updated, newLock); err != nil {
		return RemoveResult{}, fault.Wrap("project_write_failed", "DAC could not write the project files.", err)
	}
	return RemoveResult{Name: name, Version: version, AssetCount: len(updated.Assets)}, nil
}

// PathResult reports one verified object path.
type PathResult struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Digest  string `json:"digest"`
	Size    int64  `json:"size"`
	Path    string `json:"path"`
}

// Path returns one verified object path.
func (service *Service) Path(name, version string) (PathResult, error) {
	_, lock, err := service.readProject()
	if err != nil {
		return PathResult{}, err
	}
	asset, exists := lock.Assets[name]
	if !exists || asset.Version != version {
		return PathResult{}, fault.New("asset_unknown", "The project does not have this asset coordinate.")
	}
	valid, err := service.cached(Object{Digest: asset.Digest, Size: asset.Size})
	if err != nil {
		return PathResult{}, err
	}
	if !valid {
		return PathResult{}, fault.New("cache_object_invalid", "The cache object is missing. Run dac pull.")
	}
	path, err := service.objectPath(asset.Digest)
	if err != nil {
		return PathResult{}, err
	}
	return PathResult{Name: name, Version: version, Digest: asset.Digest, Size: asset.Size, Path: path}, nil
}

// GCOptions controls one cache collection.
type GCOptions struct {
	MaxAge time.Duration
	DryRun bool
}

// GCResult reports the objects that a collection removed or would remove.
type GCResult struct {
	Digests     []string `json:"digests"`
	ObjectCount int      `json:"objectCount"`
	ByteCount   int64    `json:"byteCount"`
	TempCount   int      `json:"tempCount"`
	DryRun      bool     `json:"dryRun"`
}

// CacheGC removes cache objects that nothing has used recently. It does not
// read the project files: the cache is shared by every project on the machine.
func (service *Service) CacheGC(ctx context.Context, options GCOptions) (GCResult, error) {
	result, err := service.Store.GC(ctx, options)
	if err != nil {
		return GCResult{}, fault.Wrap("cache_gc_failed", "DAC could not collect the cache.", err)
	}
	if result.Digests == nil {
		result.Digests = []string{}
	}
	return result, nil
}
