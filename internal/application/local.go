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
	// Unlocked remains for result compatibility. Remove leaves the complete lock file untouched.
	Unlocked []string `json:"unlocked"`
}

// Remove deletes one exact coordinate from the manifest without reading or writing the lock file.
// This keeps the command usable offline even when the lock is missing, stale, or invalid.
func (service *Service) Remove(name coord.Coordinate) (RemoveResult, error) {
	manifest, err := service.readManifest()
	if err != nil {
		return RemoveResult{}, err
	}
	if _, exists := manifest.Assets[name]; !exists {
		return RemoveResult{}, unknownCoordinate(name, manifest.Assets)
	}
	updated := manifest.Clone()
	delete(updated.Assets, name)
	if err := project.Write(service.ManifestPath, updated); err != nil {
		return RemoveResult{}, fault.Wrap("project_write_failed", "DAC could not write the manifest file.", err)
	}
	return RemoveResult{
		Coordinate: name.String(),
		Namespace:  name.Namespace,
		Name:       name.Name,
		Version:    name.Version,
		AssetCount: len(updated.Assets),
		Remaining:  coord.Versions(coord.InGroup(updated.Assets, name.Group())),
		Unlocked:   []string{},
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
	// Filename is what DAC called these bytes when it stored them, which is the only thing left
	// saying what an object is once no project names it.
	Filename string `json:"filename,omitempty"`
	// SourceURL is where the bytes came from.
	SourceURL string `json:"sourceUrl,omitempty"`
	// KnownAs names every coordinate the catalog has recorded for this object. It is a weaker
	// claim than Coordinates: those resolve to this object now, these once did.
	KnownAs []string `json:"knownAs,omitempty"`
	// FirstSeen is when DAC first stored these bytes, and the zero time when nothing recorded it.
	FirstSeen time.Time `json:"firstSeen"`
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
	// UnknownCount counts the objects nothing can say anything about, which is the number the
	// catalog exists to drive to zero.
	UnknownCount int `json:"unknownCount"`
}

// CacheList reports the objects in the cache, newest use first.
//
// Listing the whole cache asks about the cache rather than about a project, so it answers
// wherever it is run. A directory with no manifest is not an unusual place to ask what the cache
// is holding; it is the usual one, because deleting a project is what leaves objects behind that
// nothing accounts for.
func (service *Service) CacheList(ctx context.Context, options CacheListOptions) (CacheListResult, error) {
	// One read serves both the target list and the owner map: a single command cannot see the project change.
	manifest, lock, err := service.readProject()
	if err != nil {
		if !options.All {
			return CacheListResult{}, err
		}
		// A listing of the whole cache carries on without a project, and reports no coordinates
		// of its own. What the objects are is the catalog's answer to give. The listing below
		// settles err again, because reaching here means it is about to list the whole cache.
		manifest, lock = project.Manifest{}, project.Lock{}
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
		object := CacheObject{ObjectDescription: description, Coordinates: owners[value]}
		if record, known := service.describe(value); known {
			object.Filename, object.SourceURL = record.Filename, record.SourceURL
			object.KnownAs, object.FirstSeen = record.KnownAs, record.FirstSeen
		} else if len(object.Coordinates) == 0 {
			result.UnknownCount++
		}
		result.Objects = append(result.Objects, object)
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
	service.forget(result.Digests)
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
	// A record describes what the cache holds, so it goes when the object does. A dry run
	// removed nothing and so forgets nothing.
	if !options.DryRun {
		service.forget(result.Digests)
	}
	return result, nil
}
