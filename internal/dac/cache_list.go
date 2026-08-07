package dac

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
)

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
