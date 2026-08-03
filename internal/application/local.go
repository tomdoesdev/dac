package application

// This file holds the commands that resolve nothing: they read or write project
// state, or collect the cache, and never touch the network.

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/tom/dac/internal/coord"
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

// RemoveResult reports the asset removed from a project.
type RemoveResult struct {
	Coordinate string `json:"coordinate"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	AssetCount int    `json:"assetCount"`
	// Remaining names the versions of this asset the project still has. A
	// removal used to take the whole asset with it, and now it takes one
	// version, so the command has to say which of the two it did.
	Remaining []string `json:"remaining"`
	// Unlocked names the assets the lock file no longer describes once this
	// removal has written it. A removal resolves nothing, so a manifest a hand
	// edit already left stale stays stale, and this is how the command says so
	// instead of implying it settled a project it did not.
	Unlocked []string `json:"unlocked"`
}

// Remove deletes one exact coordinate without a network request.
//
// It rebuilds the lock file rather than requiring a current one: dropping an
// asset is the one project change that needs nothing from an origin, so it
// works on a project whose lock file is missing or stale. What it will not do
// is resolve the rest to make them agree, because that would put a network
// request behind a command documented not to make one.
func (service *Service) Remove(name coord.Coordinate) (RemoveResult, error) {
	manifest, err := service.readManifest()
	if err != nil {
		return RemoveResult{}, err
	}
	lock, err := service.readLockIfPresent()
	if err != nil {
		return RemoveResult{}, err
	}
	if _, exists := manifest.Assets[name]; !exists {
		return RemoveResult{}, unknownCoordinate(name, manifest.Assets)
	}
	updated := manifest.Clone()
	delete(updated.Assets, name)
	// Building from the manifest rather than deleting from the lock also drops
	// entries for assets the manifest no longer names at all.
	assets := make(map[coord.Coordinate]project.LockAsset, len(updated.Assets))
	unlocked := make([]string, 0, len(updated.Assets))
	for _, other := range updated.Coordinates() {
		locked, found := lock.Assets[other]
		if !project.Agrees(updated.Assets[other], locked, found) {
			unlocked = append(unlocked, other.String())
			continue
		}
		assets[other] = locked
	}
	updatedLock, err := newLock(updated, assets)
	if err != nil {
		return RemoveResult{}, err
	}
	if err := project.WritePair(service.ManifestPath, service.LockPath, updated, updatedLock); err != nil {
		return RemoveResult{}, fault.Wrap("project_write_failed", "DAC could not write the project files.", err)
	}
	return RemoveResult{
		Coordinate: name.String(),
		Namespace:  name.Namespace,
		Name:       name.Name,
		Version:    name.Version,
		AssetCount: len(updated.Assets),
		Remaining:  coord.Versions(coord.InGroup(updated.Assets, name.Group())),
		Unlocked:   unlocked,
	}, nil
}

// PathResult reports one verified object path.
type PathResult struct {
	Coordinate string `json:"coordinate"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Digest     string `json:"digest"`
	Size       int64  `json:"size"`
	Path       string `json:"path"`
}

// Path returns one verified object path.
//
// It takes a selection rather than a coordinate so that the version can be left
// off, which is the common case at a shell prompt and in a script for a project
// that carries one version of a thing. It is answered only when the project
// leaves nothing to choose between; see onlyAsset.
func (service *Service) Path(selection Selection) (PathResult, error) {
	_, lock, err := service.readProject()
	if err != nil {
		return PathResult{}, err
	}
	name, err := onlyAsset(selection, lock.Assets)
	if err != nil {
		return PathResult{}, err
	}
	asset := lock.Assets[name]
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
	return PathResult{
		Coordinate: name.String(),
		Namespace:  name.Namespace,
		Name:       name.Name,
		Version:    name.Version,
		Digest:     asset.Digest,
		Size:       asset.Size,
		Path:       path,
	}, nil
}

// GCOptions controls one cache collection.
type GCOptions struct {
	MaxAge time.Duration
	DryRun bool
}

// GCResult reports the objects that a collection removed or would remove.
type GCResult struct {
	Digests      []string `json:"digests"`
	ObjectCount  int      `json:"objectCount"`
	ByteCount    int64    `json:"byteCount"`
	TempCount    int      `json:"tempCount"`
	SidecarCount int      `json:"sidecarCount"`
	DryRun       bool     `json:"dryRun"`
}

// CacheDirResult reports where the object cache resolved to. DAC picks that
// directory from four sources, and until now the only way to find out which one
// won was to look at a path printed beside an asset that happened to be cached.
type CacheDirResult struct {
	Path string `json:"path"`
}

// VerifyCacheOptions controls one explicit cache check.
type VerifyCacheOptions struct {
	// All checks every object in the cache instead of the ones this project
	// locked. It reads no project files, because the cache is shared.
	All bool
	// Repair removes the objects that fail. A corrupt object is worth nothing:
	// leaving it in place only means the next command has to rediscover it.
	Repair bool
}

// VerifyCacheResult reports one explicit cache check.
type VerifyCacheResult struct {
	Checked      int      `json:"checked"`
	ByteCount    int64    `json:"byteCount"`
	CorruptCount int      `json:"corruptCount"`
	MissingCount int      `json:"missingCount"`
	Repaired     int      `json:"repaired"`
	Corrupt      []string `json:"corrupt"`
	Missing      []string `json:"missing"`
}

// VerifyCache hashes cache objects and reports the ones that no longer match
// the digest that names them.
//
// Ordinary commands avoid this work: they compare an object against the sidecar
// that recorded it at install time, which catches anything that has written to
// the object but not a disk that quietly changed the bytes underneath. This is
// the command that answers the second question, at the cost of reading
// everything it checks.
func (service *Service) VerifyCache(ctx context.Context, options VerifyCacheOptions) (VerifyCacheResult, error) {
	digests, err := service.verifyTargets(ctx, options.All)
	if err != nil {
		return VerifyCacheResult{}, err
	}
	result := VerifyCacheResult{Corrupt: []string{}, Missing: []string{}}
	for _, value := range digests {
		if err := ctx.Err(); err != nil {
			return VerifyCacheResult{}, fault.Wrap("cancelled", "The command was cancelled.", err)
		}
		object, found, err := service.Store.Verify(ctx, value)
		result.ByteCount += object.Size
		switch {
		case err != nil && corrupted(err):
			result.Corrupt = append(result.Corrupt, value)
			if options.Repair {
				if err := service.Store.Remove(ctx, value); err != nil {
					return VerifyCacheResult{}, fault.Wrap("cache_repair_failed", "DAC could not remove the corrupt cache object.", err)
				}
				result.Repaired++
			}
		case err != nil:
			return VerifyCacheResult{}, cacheReadError(err)
		case !found:
			result.Missing = append(result.Missing, value)
		}
		result.Checked++
	}
	result.CorruptCount = len(result.Corrupt)
	result.MissingCount = len(result.Missing)
	return result, nil
}

// verifyTargets returns the digests one check covers: every object in the
// shared cache, or the ones this project locked.
func (service *Service) verifyTargets(ctx context.Context, all bool) ([]string, error) {
	if all {
		digests, err := service.Store.List(ctx)
		if err != nil {
			return nil, fault.Wrap("cache_read_failed", "DAC could not list the cache.", err)
		}
		return digests, nil
	}
	manifest, lock, err := service.readProject()
	if err != nil {
		return nil, err
	}
	digests := make([]string, 0, len(lock.Assets))
	for _, name := range manifest.Coordinates() {
		digests = append(digests, lock.Assets[name].Digest)
	}
	return digests, nil
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
