package dac

import (
	"context"

	"github.com/tomdoesdev/dac/internal/fault"
)

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
