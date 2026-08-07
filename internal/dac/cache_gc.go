package dac

import (
	"context"
	"time"

	"github.com/tomdoesdev/dac/internal/fault"
)

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
