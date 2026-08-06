package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fs/atomic"
)

// temporaryGrace protects files that another process may still be writing.
// Cleanup is deliberately best-effort: an abandoned file left this run is safe
// to collect on the next one, while removing a live download would fail it.
const temporaryGrace = time.Hour

// cacheObject records one snapshot entry and whether this run has accounted for it.
type cacheObject struct {
	digest string
	path   string
	size   int64
	used   time.Time
	done   bool
}

// cacheSidecar is a metadata file GC may need to remove after its object is gone.
type cacheSidecar struct {
	digest     string
	path       string
	objectPath string
	modified   time.Time
}

// cacheTemporary is an interrupted atomic write or download.
type cacheTemporary struct {
	path     string
	modified time.Time
}

// cacheInventory contains one stable view of the files a collection considers.
type cacheInventory struct {
	objects     []cacheObject
	sidecars    []cacheSidecar
	temporaries []cacheTemporary
	totalSize   int64
}

// collection reports the outcome of rechecking and removing one object.
type collection struct {
	size    int64
	removed bool
	present bool
}

// GC removes expired objects, evicts least-recently-used survivors to satisfy a
// size bound, and cleans abandoned cache files. Every destructive decision is
// rechecked while holding the object's digest lock.
func (store *Store) GC(ctx context.Context, options application.GCOptions) (application.GCResult, error) {
	if options.MaxAge < 0 {
		return application.GCResult{}, errors.New("the maximum age must not be negative")
	}

	now := time.Now()
	cutoff := now.Add(-options.MaxAge)
	temporaryCutoff := now.Add(-temporaryGrace)
	if !options.All {
		temporaryCutoff = earlier(cutoff, temporaryCutoff)
	}
	result := application.GCResult{Digests: []string{}, DryRun: options.DryRun}

	inventory, err := store.scanCache(ctx)
	if err != nil {
		return application.GCResult{}, err
	}
	slices.SortFunc(inventory.objects, compareCacheObjects)

	// Expiration runs before eviction because removing unused objects may satisfy
	// the size bound without discarding anything still in use.
	for index := range inventory.objects {
		object := &inventory.objects[index]
		if !options.All && !object.used.Before(cutoff) {
			continue
		}
		outcome, err := store.collectObject(ctx, *object, options.DryRun, func(used time.Time) bool {
			return options.All || used.Before(cutoff)
		})
		if err != nil {
			return application.GCResult{}, err
		}
		if !outcome.present {
			object.done = true
			inventory.totalSize -= object.size
			continue
		}
		if outcome.removed {
			object.done = true
			inventory.totalSize -= object.size
			addCollection(&result, object.digest, outcome.size)
		}
	}

	// Clearing already selects every object, and zero means no size bound.
	if options.MaxSize > 0 && !options.All {
		for index := range inventory.objects {
			if inventory.totalSize <= options.MaxSize {
				break
			}
			object := &inventory.objects[index]
			if object.done {
				continue
			}
			observed := object.used
			outcome, err := store.collectObject(ctx, *object, options.DryRun, func(used time.Time) bool {
				return !used.After(observed)
			})
			if err != nil {
				return application.GCResult{}, err
			}
			if !outcome.present {
				object.done = true
				inventory.totalSize -= object.size
				continue
			}
			if outcome.removed {
				object.done = true
				inventory.totalSize -= object.size
				addCollection(&result, object.digest, outcome.size)
				store.trace().Debug("evicted", "digest", object.digest, "size", outcome.size,
					"lastUsed", object.used, "remaining", inventory.totalSize, "bound", options.MaxSize)
			}
		}
	}

	if err := store.cleanCache(ctx, inventory, options, cutoff, temporaryCutoff); err != nil {
		return application.GCResult{}, err
	}
	slices.Sort(result.Digests)
	return result, nil
}

// scanCache reads each cache directory once and records only files DAC owns.
// Unknown names and directories are left alone so GC cannot remove user data.
func (store *Store) scanCache(ctx context.Context) (cacheInventory, error) {
	var inventory cacheInventory
	blobs := filepath.Join(store.Root, "blobs", "sha256")
	entries, err := os.ReadDir(blobs)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return cacheInventory{}, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return cacheInventory{}, err
		}
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		path := filepath.Join(blobs, name)
		switch {
		case strings.HasPrefix(name, atomic.DefaultTempPrefix):
			info, found, err := currentInfo(entry)
			if err != nil {
				return cacheInventory{}, err
			}
			if found {
				inventory.temporaries = append(inventory.temporaries, cacheTemporary{path: path, modified: info.ModTime()})
			}
		case strings.HasSuffix(name, metaSuffix):
			objectName := strings.TrimSuffix(name, metaSuffix)
			value := digest.Prefix + objectName
			if _, err := digest.Hex(value); err != nil {
				continue
			}
			info, found, err := currentInfo(entry)
			if err != nil {
				return cacheInventory{}, err
			}
			if found {
				inventory.sidecars = append(inventory.sidecars, cacheSidecar{
					digest: value, path: path, objectPath: filepath.Join(blobs, objectName), modified: info.ModTime(),
				})
			}
		default:
			value := digest.Prefix + name
			if _, err := digest.Hex(value); err != nil {
				continue
			}
			info, found, err := currentInfo(entry)
			if err != nil {
				return cacheInventory{}, err
			}
			if !found {
				continue
			}
			used, err := store.lastUsed(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return cacheInventory{}, err
			}
			inventory.objects = append(inventory.objects, cacheObject{digest: value, path: path, size: info.Size(), used: used})
			inventory.totalSize += info.Size()
		}
	}

	temporaryDirectory := filepath.Join(store.Root, "tmp")
	entries, err = os.ReadDir(temporaryDirectory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return cacheInventory{}, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return cacheInventory{}, err
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "download-") {
			continue
		}
		info, found, err := currentInfo(entry)
		if err != nil {
			return cacheInventory{}, err
		}
		if found {
			inventory.temporaries = append(inventory.temporaries, cacheTemporary{
				path: filepath.Join(temporaryDirectory, entry.Name()), modified: info.ModTime(),
			})
		}
	}
	return inventory, nil
}

// currentInfo treats disappearance during a directory scan as an ordinary race.
func currentInfo(entry os.DirEntry) (os.FileInfo, bool, error) {
	info, err := entry.Info()
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return info, true, nil
}

// compareCacheObjects orders eviction candidates deterministically, oldest first.
func compareCacheObjects(left, right cacheObject) int {
	if left.used.Equal(right.used) {
		return strings.Compare(left.digest, right.digest)
	}
	if left.used.Before(right.used) {
		return -1
	}
	return 1
}

// addCollection records an object selected by either expiration or eviction.
func addCollection(result *application.GCResult, value string, size int64) {
	result.Digests = append(result.Digests, value)
	result.ObjectCount++
	result.ByteCount += size
}

// collectObject removes one eligible object under its digest lock. The second
// eligibility check prevents GC from removing an object used while it waited.
func (store *Store) collectObject(ctx context.Context, object cacheObject, dryRun bool, eligible func(time.Time) bool) (collection, error) {
	var outcome collection
	err := store.WithLock(ctx, object.digest, func() error {
		info, err := os.Stat(object.path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		outcome.present = true
		used, err := store.lastUsed(object.path)
		if errors.Is(err, os.ErrNotExist) {
			outcome.present = false
			return nil
		}
		if err != nil {
			return err
		}
		if !eligible(used) {
			return nil
		}
		outcome.size = info.Size()
		outcome.removed = true
		if dryRun {
			return nil
		}
		return store.deleteObject(object.digest, object.path)
	})
	return outcome, err
}

// cleanCache removes housekeeping files without exposing them as collected
// objects. Objects and sidecars remain independently safe under concurrent GC.
func (store *Store) cleanCache(ctx context.Context, inventory cacheInventory, options application.GCOptions, cutoff, temporaryCutoff time.Time) error {
	for _, temporary := range inventory.temporaries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !temporary.modified.Before(temporaryCutoff) {
			continue
		}
		if err := removeTemporary(temporary.path, temporaryCutoff, options.DryRun); err != nil {
			return err
		}
	}
	for _, sidecar := range inventory.sidecars {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !options.All && !sidecar.modified.Before(cutoff) {
			continue
		}
		if err := store.removeOrphanedSidecar(ctx, sidecar, options.DryRun); err != nil {
			return err
		}
	}
	return nil
}

// removeTemporary rechecks the timestamp so a path reused since the inventory
// scan cannot cause GC to delete a new in-flight write.
func removeTemporary(path string, cutoff time.Time, dryRun bool) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.ModTime().Before(cutoff) || dryRun {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// removeOrphanedSidecar removes metadata only after its object is confirmed
// absent under the same digest lock used by object installation.
func (store *Store) removeOrphanedSidecar(ctx context.Context, sidecar cacheSidecar, dryRun bool) error {
	return store.WithLock(ctx, sidecar.digest, func() error {
		if _, err := os.Stat(sidecar.objectPath); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if dryRun {
			return nil
		}
		if err := os.Remove(sidecar.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

// lastUsed reports when a project last referenced an object. Legacy objects
// without sidecars retain the object timestamp behavior of the old format.
func (store *Store) lastUsed(objectPath string) (time.Time, error) {
	info, err := os.Stat(metaPath(objectPath))
	if err == nil {
		return info.ModTime(), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return time.Time{}, err
	}
	info, err = os.Stat(objectPath)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

// earlier returns the cutoff that preserves more recent files.
func earlier(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}
	return second
}
