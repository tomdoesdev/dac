package application

// This file holds the commands that resolve nothing: they read or write project state, or collect the cache, and never touch the network.

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
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
	// Remaining names the versions of this asset the project still has.
	Remaining []string `json:"remaining"`
	// Unlocked names the assets the lock file no longer describes once this removal has written it.
	Unlocked []string `json:"unlocked"`
}

// Remove deletes one exact coordinate without a network request.
// Remove rebuilds a partial lock because asset deletion needs no origin request.
func (service *Service) Remove(name coord.Coordinate) (RemoveResult, error) {
	manifest, err := service.readManifest()
	if err != nil {
		return RemoveResult{}, err
	}
	lock, _, err := service.readLockIfPresent()
	if err != nil {
		return RemoveResult{}, err
	}
	if _, exists := manifest.Assets[name]; !exists {
		return RemoveResult{}, unknownCoordinate(name, manifest.Assets)
	}
	updated := manifest.Clone()
	delete(updated.Assets, name)
	// Building from the manifest rather than deleting from the lock also drops entries for assets the manifest no longer names at all.
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
// Path accepts a group when the project has one matching version.
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
	// MaxSize bounds what the cache may hold once collection has run.
	// The bound applies during collection, so one download can exceed it until the next run.
	MaxSize int64
	DryRun  bool
	// All removes every object regardless of age.
	All bool
}

// GCResult reports the objects that a collection removed or would remove.
type GCResult struct {
	Digests     []string `json:"digests"`
	ObjectCount int      `json:"objectCount"`
	ByteCount   int64    `json:"byteCount"`
	DryRun      bool     `json:"dryRun"`
}

// ConfigPathResult reports the config files one run read, most important first.
type ConfigPathResult struct {
	Files []string `json:"files"`
}

// ConfigSetting is one effective setting and the file that supplied it.
type ConfigSetting struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// ConfigShowResult reports the effective configuration.
type ConfigShowResult struct {
	Files    []string        `json:"files"`
	Settings []ConfigSetting `json:"settings"`
}

// CacheDirResult reports where the object cache resolved to.
type CacheDirResult struct {
	Path string `json:"path"`
}

// TrustPathResult reports where the trusted-hosts file resolved to.
type TrustPathResult struct {
	Path string `json:"path"`
}

// TrustHost is one trusted host and what is known about when it was used.
type TrustHost struct {
	Host    string    `json:"host"`
	AddedAt time.Time `json:"addedAt"`
	// LastUsed is the zero time until a download reaches this host.
	LastUsed time.Time `json:"lastUsed"`
}

// TrustListResult reports the trusted hosts, in name order.
type TrustListResult struct {
	Path  string      `json:"path"`
	Hosts []TrustHost `json:"hosts"`
}

// TrustAddResult reports what one trust addition changed.
type TrustAddResult struct {
	Path  string   `json:"path"`
	Added []string `json:"added"`
	// Present names the hosts that were already trusted, so adding twice is not a failure.
	Present []string `json:"present"`
}

// TrustRemoveResult reports what one trust removal changed.
type TrustRemoveResult struct {
	Path    string   `json:"path"`
	Removed []string `json:"removed"`
	// Absent names the hosts that were not trusted to begin with.
	Absent []string `json:"absent"`
}

// TrustGCResult reports the hosts a trust collection dropped.
type TrustGCResult struct {
	Path      string   `json:"path"`
	Hosts     []string `json:"hosts"`
	HostCount int      `json:"hostCount"`
	DryRun    bool     `json:"dryRun"`
}

// VerifyCacheOptions controls one explicit cache check.
type VerifyCacheOptions struct {
	// All checks every object in the cache instead of the ones this project locked.
	All bool
	// Repair removes the objects that fail.
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

// VerifyCache hashes cache objects and reports the ones that no longer match the digest that names them.
// Ordinary commands trust unchanged sidecars; VerifyCache detects silent storage damage.
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

// verifyTargets returns the digests one check covers: every object in the shared cache, or the ones this project locked.
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
	return lockedDigests(manifest, lock), nil
}

// lockedDigests returns the digest of every locked asset, in manifest order.
func lockedDigests(manifest project.Manifest, lock project.Lock) []string {
	digests := make([]string, 0, len(lock.Assets))
	for _, name := range manifest.Coordinates() {
		digests = append(digests, lock.Assets[name].Digest)
	}
	return digests
}

// ObjectDescription reports what the cache holds for one digest.
type ObjectDescription struct {
	Digest   string    `json:"digest"`
	Size     int64     `json:"size"`
	LastUsed time.Time `json:"lastUsed"`
}

// CacheObject is one entry in a cache listing.
type CacheObject struct {
	ObjectDescription
	// Coordinates names the project assets that resolve to this object.
	Coordinates []string `json:"coordinates,omitempty"`
}

// CacheListOptions controls one cache listing.
type CacheListOptions struct {
	// All lists every object in the cache instead of the ones this project locked.
	All bool
}

// CacheListResult reports what the cache holds.
type CacheListResult struct {
	Objects     []CacheObject `json:"objects"`
	ObjectCount int           `json:"objectCount"`
	ByteCount   int64         `json:"byteCount"`
	// MissingCount counts the locked objects this project does not have.
	MissingCount int `json:"missingCount"`
}

// CacheList reports the objects in the cache, newest use first.
func (service *Service) CacheList(ctx context.Context, options CacheListOptions) (CacheListResult, error) {
	// One read serves both the target list and the owner map: a single command cannot see the project change.
	manifest, lock, err := service.readProject()
	if err != nil {
		return CacheListResult{}, err
	}
	digests := lockedDigests(manifest, lock)
	if options.All {
		if digests, err = service.Store.List(ctx); err != nil {
			return CacheListResult{}, fault.Wrap("cache_read_failed", "DAC could not list the cache.", err)
		}
	}
	owners := objectOwners(manifest, lock)
	result := CacheListResult{Objects: []CacheObject{}}
	seen := map[string]struct{}{}
	for _, value := range digests {
		if err := ctx.Err(); err != nil {
			return CacheListResult{}, fault.Wrap("cancelled", "The command was cancelled.", err)
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		description, found, err := service.Store.Describe(value)
		if err != nil {
			return CacheListResult{}, cacheReadError(err)
		}
		if !found {
			result.MissingCount++
			continue
		}
		result.Objects = append(result.Objects, CacheObject{ObjectDescription: description, Coordinates: owners[value]})
		result.ObjectCount++
		result.ByteCount += description.Size
	}
	// Newest first, because the question a listing answers before any other is what collection is about to take, and that is the other end.
	slices.SortFunc(result.Objects, func(left, right CacheObject) int {
		if left.LastUsed.Equal(right.LastUsed) {
			return strings.Compare(left.Digest, right.Digest)
		}
		if left.LastUsed.After(right.LastUsed) {
			return -1
		}
		return 1
	})
	return result, nil
}

// objectOwners maps each locked digest to the coordinates that resolve to it.
// It takes the project its caller has already read, because re-reading it would parse, normalize and
// re-validate every asset a second time within one command that cannot see the file change.
func objectOwners(manifest project.Manifest, lock project.Lock) map[string][]string {
	owners := map[string][]string{}
	for _, name := range manifest.Coordinates() {
		locked, exists := lock.Assets[name]
		if !exists {
			continue
		}
		owners[locked.Digest] = append(owners[locked.Digest], name.String())
	}
	for _, names := range owners {
		slices.Sort(names)
	}
	return owners
}

// CacheRemoveOptions controls the removal of specific assets' objects.
type CacheRemoveOptions struct {
	Coordinates []coord.Coordinate
	// Force accepts a removal that also takes bytes belonging to an asset the caller did not name.
	Force bool
}

// CacheRemoveResult reports one targeted removal.
type CacheRemoveResult struct {
	Digests     []string `json:"digests"`
	ObjectCount int      `json:"objectCount"`
	ByteCount   int64    `json:"byteCount"`
	// Shared names the coordinates that lost their bytes without being asked for, which --force is the permission to do.
	Shared []string `json:"shared"`
	// Missing names the requested coordinates the cache did not hold.
	Missing []string `json:"missing"`
}

// CacheRemove drops the objects specific assets resolved to.
func (service *Service) CacheRemove(ctx context.Context, options CacheRemoveOptions) (CacheRemoveResult, error) {
	manifest, lock, err := service.readProject()
	if err != nil {
		return CacheRemoveResult{}, err
	}
	owners := objectOwners(manifest, lock)
	result := CacheRemoveResult{Digests: []string{}, Shared: []string{}, Missing: []string{}}
	named := map[string]struct{}{}
	targets := map[string]struct{}{}
	for _, name := range options.Coordinates {
		if _, exists := manifest.Assets[name]; !exists {
			return CacheRemoveResult{}, &fault.Error{
				Code:    "asset_not_found",
				Message: "The project does not have that asset version.",
				Details: map[string]any{"asset": name.String()},
			}
		}
		locked, exists := lock.Assets[name]
		if !exists {
			return CacheRemoveResult{}, &fault.Error{
				Code:    "lock_stale",
				Message: "The lock file does not describe that asset. Run dac pull --refresh.",
				Details: map[string]any{"asset": name.String()},
			}
		}
		named[name.String()] = struct{}{}
		targets[locked.Digest] = struct{}{}
	}
	for value := range targets {
		for _, owner := range owners[value] {
			if _, asked := named[owner]; !asked {
				result.Shared = append(result.Shared, owner)
			}
		}
	}
	slices.Sort(result.Shared)
	if len(result.Shared) > 0 && !options.Force {
		return CacheRemoveResult{}, &fault.Error{
			Code:    "cache_object_shared",
			Message: "Removing these objects would also uncache assets that were not named. Use --force to accept that.",
			Details: map[string]any{"shared": result.Shared},
		}
	}
	for value := range targets {
		if err := ctx.Err(); err != nil {
			return CacheRemoveResult{}, fault.Wrap("cancelled", "The command was cancelled.", err)
		}
		description, found, err := service.Store.Describe(value)
		if err != nil {
			return CacheRemoveResult{}, cacheReadError(err)
		}
		if !found {
			result.Missing = append(result.Missing, value)
			continue
		}
		if err := service.Store.Remove(ctx, value); err != nil {
			return CacheRemoveResult{}, fault.Wrap("cache_remove_failed", "DAC could not remove the cache object.", err)
		}
		result.Digests = append(result.Digests, value)
		result.ObjectCount++
		result.ByteCount += description.Size
	}
	slices.Sort(result.Digests)
	slices.Sort(result.Missing)
	return result, nil
}

// CacheClear removes every object in the cache.
// CacheClear covers the shared digest cache because objects do not belong to one project.
func (service *Service) CacheClear(ctx context.Context, dryRun bool) (GCResult, error) {
	return service.CacheGC(ctx, GCOptions{DryRun: dryRun, All: true})
}

// CacheGC removes cache objects that nothing has used recently.
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
