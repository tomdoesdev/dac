package dac

import (
	"context"
	"slices"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/fault"
)

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
