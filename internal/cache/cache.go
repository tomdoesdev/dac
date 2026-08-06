// Package cache stores immutable content-addressed objects.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/debug"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fs/atomic"
	"github.com/tomdoesdev/dac/internal/fs/flock"
)

// Store manages one filesystem cache.
// A store must not be copied: verified carries the objects this process has already hashed, and a copy would hash them all over again.
type Store struct {
	Root string
	// Logger traces what the cache answered for each object.
	Logger *slog.Logger
	// verified maps a digest to the file information of the bytes this process hashed for it.
	verified sync.Map
}

// trace returns the logger for this store, which discards when nothing set one.
func (store *Store) trace() *slog.Logger { return debug.Or(store.Logger) }

// ResolveRoot returns an absolute cache root.
func ResolveRoot(option string) (string, error) {
	selected := option
	if selected == "" {
		if value := os.Getenv("XDG_CACHE_HOME"); filepath.IsAbs(value) {
			selected = filepath.Join(value, "dac")
		}
	}
	if selected == "" {
		home, err := os.UserHomeDir()
		if err != nil || !filepath.IsAbs(home) {
			return "", errors.New("set --cache-dir, DAC_CACHE_DIR, XDG_CACHE_HOME, or HOME")
		}
		selected = filepath.Join(home, ".cache", "dac")
	}
	return filepath.Abs(selected)
}

// New creates a store without creating its root.
func New(root string) *Store { return &Store{Root: root} }

// Path returns the path for a valid digest.
func (store *Store) Path(value string) (string, error) {
	hexValue, err := digest.Hex(value)
	if err != nil {
		return "", err
	}
	return filepath.Join(store.Root, "blobs", "sha256", hexValue), nil
}

// Stat returns the object stored for a digest and confirms that it still holds the bytes DAC installed.
// It usually reads no object bytes at all: see check in meta.go.
func (store *Store) Stat(value string) (application.Object, bool, error) {
	path, err := store.Path(value)
	if err != nil {
		return application.Object{}, false, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		store.trace().Debug("cache miss", "digest", value)
		return application.Object{}, false, nil
	}
	if err != nil {
		return application.Object{}, false, err
	}
	if err := store.check(value, path, info); err != nil {
		store.trace().Debug("cache object failed its check", "digest", value, "error", err)
		return application.Object{}, false, err
	}
	touch(metaPath(path))
	store.trace().Debug("cache hit", "digest", value, "size", info.Size())
	return application.Object{Digest: value, Size: info.Size()}, true, nil
}

// GC removes old objects, abandoned temporary files, and orphaned sidecars.
// Sidecars record cache use without changing the object timestamp that proves immutability.
func (store *Store) GC(ctx context.Context, options application.GCOptions) (application.GCResult, error) {
	if options.MaxAge < 0 {
		return application.GCResult{}, errors.New("the maximum age must not be negative")
	}
	result := application.GCResult{Digests: []string{}, DryRun: options.DryRun}
	cutoff := time.Now().Add(-options.MaxAge)
	if options.All {
		// Everything is older than a cutoff in the far future.
		cutoff = time.Now().Add(farFuture)
	}
	// A temporary file is never taken on the age cutoff alone. See temporaryGrace.
	temporaryCutoff := earlier(cutoff, time.Now().Add(-temporaryGrace))
	// collected names the objects this run took, so the sidecar sweep can tell the pairs it removed itself from the sidecars that were already alone.
	collected := map[string]struct{}{}

	blobs := filepath.Join(store.Root, "blobs", "sha256")
	entries, err := os.ReadDir(blobs)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return application.GCResult{}, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return application.GCResult{}, err
		}
		if entry.IsDir() || strings.HasSuffix(entry.Name(), metaSuffix) {
			continue
		}
		value := digest.Prefix + entry.Name()
		if _, err := digest.Hex(value); err != nil {
			continue
		}
		used, err := store.lastUsed(filepath.Join(blobs, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return application.GCResult{}, err
		}
		if !used.Before(cutoff) {
			continue
		}
		size, removed, err := store.collect(ctx, value, olderThan(cutoff), options.DryRun)
		if err != nil {
			return application.GCResult{}, err
		}
		if removed {
			result.Digests = append(result.Digests, value)
			result.ObjectCount++
			result.ByteCount += size
			collected[entry.Name()] = struct{}{}
		}
	}
	slices.Sort(result.Digests)

	temporary, err := store.collectTemporary(temporaryCutoff, options.DryRun)
	if err != nil {
		return application.GCResult{}, err
	}
	result.TempCount = temporary

	sidecars, abandoned, err := store.collectSidecars(ctx, blobs, entries, collected, cutoff, temporaryCutoff, options.DryRun)
	if err != nil {
		return application.GCResult{}, err
	}
	result.SidecarCount = sidecars
	result.TempCount += abandoned

	// Clearing takes everything regardless, so there is no bound left to be over.
	if options.MaxSize > 0 && !options.All {
		if err := store.evict(ctx, options, collected, &result); err != nil {
			return application.GCResult{}, err
		}
	}
	return result, nil
}

// olderThan is the collection question: has nothing used this object since the cutoff.
func olderThan(cutoff time.Time) func(time.Time) bool {
	return func(used time.Time) bool { return used.Before(cutoff) }
}

// evict removes the least recently used objects until the cache is inside its size bound.
// Age and size are different questions, and a cache answers to both.
// Eviction runs after collection and removes live objects from oldest to newest.
// It is a bound on collection rather than a quota on the cache.
func (store *Store) evict(ctx context.Context, options application.GCOptions, collected map[string]struct{}, result *application.GCResult) error {
	objects, total, err := store.evictable(ctx, collected)
	if err != nil {
		return err
	}
	if total <= options.MaxSize {
		return nil
	}
	// Oldest first, and by digest between objects that share a timestamp so that two runs over one cache make the same choices.
	slices.SortFunc(objects, func(left, right candidate) int {
		if left.used.Equal(right.used) {
			return strings.Compare(left.digest, right.digest)
		}
		if left.used.Before(right.used) {
			return -1
		}
		return 1
	})
	for _, candidate := range objects {
		if total <= options.MaxSize {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// An object used while eviction waits becomes recent and is not removed.
		size, removed, err := store.collect(ctx, candidate.digest, unusedSince(candidate.used), options.DryRun)
		if err != nil {
			return err
		}
		if !removed {
			continue
		}
		store.trace().Debug("evicted", "digest", candidate.digest, "size", size,
			"lastUsed", candidate.used, "remaining", total-size, "bound", options.MaxSize)
		total -= size
		result.Digests = append(result.Digests, candidate.digest)
		result.ObjectCount++
		result.ByteCount += size
		result.EvictedCount++
		result.EvictedBytes += size
	}
	slices.Sort(result.Digests)
	return nil
}

// unusedSince is the eviction question: has nothing reached this object since the run looked at it.
func unusedSince(observed time.Time) func(time.Time) bool {
	return func(used time.Time) bool { return !used.After(observed) }
}

// stored is one object a size bound may have to take, and what decides whether it goes.
type candidate struct {
	digest string
	size   int64
	used   time.Time
}

// evictable reports the objects collection left behind and what they come to.
// It skips the ones collection took.
func (store *Store) evictable(ctx context.Context, collected map[string]struct{}) ([]candidate, int64, error) {
	blobs := filepath.Join(store.Root, "blobs", "sha256")
	entries, err := os.ReadDir(blobs)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, 0, err
	}
	var objects []candidate
	var total int64
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		if entry.IsDir() || strings.HasSuffix(entry.Name(), metaSuffix) {
			continue
		}
		if _, taken := collected[entry.Name()]; taken {
			continue
		}
		value := digest.Prefix + entry.Name()
		if _, err := digest.Hex(value); err != nil {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		used, err := store.lastUsed(filepath.Join(blobs, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, 0, err
		}
		objects = append(objects, candidate{digest: value, size: info.Size(), used: used})
		total += info.Size()
	}
	return objects, total, nil
}

// farFuture is the cutoff an unconditional collection uses.
const farFuture = 100 * 365 * 24 * time.Hour

// temporaryGrace is how long a temporary file is left alone whatever the collection age is.
// A download in flight is an open file in a directory this sweep walks, and the
// age cutoff says nothing about whether somebody is still writing to it: clearing
// the cache takes every object by design, but taking a transfer's temporary file
// out from under it fails a download that was doing nothing wrong. Nothing here is
// urgent enough to be worth that, because a leftover this run leaves is one the
// next run removes.
const temporaryGrace = time.Hour

// earlier returns whichever of two cutoffs collects less.
func earlier(first, second time.Time) time.Time {
	if first.Before(second) {
		return first
	}
	return second
}

// Describe reports an object's size and when a project last used it.
// It touches nothing.
func (store *Store) Describe(value string) (application.ObjectDescription, bool, error) {
	path, err := store.Path(value)
	if err != nil {
		return application.ObjectDescription{}, false, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return application.ObjectDescription{}, false, nil
	}
	if err != nil {
		return application.ObjectDescription{}, false, err
	}
	used, err := store.lastUsed(path)
	if err != nil {
		return application.ObjectDescription{}, false, err
	}
	return application.ObjectDescription{Digest: value, Size: info.Size(), LastUsed: used}, true, nil
}

// lastUsed reports when a project last referenced an object.
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

// collect removes one object and its sidecar while it holds that object's digest lock, so a concurrent install cannot rename a new copy into place mid-removal.
// What that re-check asks is the caller's to decide, because the two removals this store makes ask different questions of the same timestamp.
func (store *Store) collect(ctx context.Context, value string, unused func(time.Time) bool, dryRun bool) (int64, bool, error) {
	path, err := store.Path(value)
	if err != nil {
		return 0, false, err
	}
	var size int64
	var removed bool
	err = store.WithLock(ctx, value, func() error {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		used, err := store.lastUsed(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !unused(used) {
			return nil
		}
		size, removed = info.Size(), true
		if dryRun {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Remove(metaPath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		store.verified.Delete(value)
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return size, removed, nil
}

// collectSidecars removes sidecars left without an object, along with the temporary files an interrupted sidecar write leaves behind.
// Collection removes an object and its sidecar together, so one that survives on its own describes an object something outside DAC deleted.
// The entries come from a listing taken before the objects were collected, so collected names the ones this run removed.
// A temporary file answers to its own cutoff, because one that is still being written belongs to a process rather than to an age.
func (store *Store) collectSidecars(ctx context.Context, directory string, entries []os.DirEntry, collected map[string]struct{}, cutoff, temporaryCutoff time.Time, dryRun bool) (int, int, error) {
	sidecars, abandoned := 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		switch {
		case strings.HasPrefix(name, atomic.DefaultTempPrefix):
			info, err := entry.Info()
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return 0, 0, err
			}
			if !info.ModTime().Before(temporaryCutoff) {
				continue
			}
			abandoned++
			if dryRun {
				continue
			}
			if err := os.Remove(filepath.Join(directory, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return 0, 0, err
			}
		case strings.HasSuffix(name, metaSuffix):
			orphaned, err := store.collectSidecar(ctx, directory, name, collected, cutoff, dryRun)
			if err != nil {
				return 0, 0, err
			}
			if orphaned {
				sidecars++
			}
		}
	}
	return sidecars, abandoned, nil
}

// collectSidecar removes one sidecar that outlived the object it described.
// It takes that object's digest lock and re-checks under it, like every other removal in this store.
// It also leaves a sidecar the cutoff has not reached, exactly as the sweep leaves a temporary file that new.
func (store *Store) collectSidecar(ctx context.Context, directory, name string, collected map[string]struct{}, cutoff time.Time, dryRun bool) (bool, error) {
	objectName := strings.TrimSuffix(name, metaSuffix)
	if _, taken := collected[objectName]; taken {
		return false, nil
	}
	// A name this store did not write is not a sidecar, whatever it ends with, and there is no object lock to take for it.
	value := digest.Prefix + objectName
	if _, err := digest.Hex(value); err != nil {
		return false, nil
	}
	path := filepath.Join(directory, name)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.ModTime().Before(cutoff) {
		return false, nil
	}
	orphaned := false
	err = store.WithLock(ctx, value, func() error {
		if _, err := os.Stat(filepath.Join(directory, objectName)); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		orphaned = true
		if dryRun {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return orphaned, nil
}

func (store *Store) collectTemporary(cutoff time.Time, dryRun bool) (int, error) {
	directory := filepath.Join(store.Root, "tmp")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "download-") {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		count++
		if dryRun {
			continue
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
	}
	return count, nil
}

// Verify hashes one object and reports what it holds.
// It deliberately ignores both the sidecar and the in-process record of what this run has already hashed.
func (store *Store) Verify(ctx context.Context, value string) (application.Object, bool, error) {
	path, err := store.Path(value)
	if err != nil {
		return application.Object{}, false, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return application.Object{}, false, nil
	} else if err != nil {
		return application.Object{}, false, err
	}
	actual, info, err := hashFile(ctx, path)
	if err != nil {
		return application.Object{}, false, err
	}
	// The size comes back even when the digest does not match, because the check read those bytes and a summary that reports how much it read should count them.
	if actual != value {
		return application.Object{Digest: value, Size: info.Size()}, true,
			&application.CorruptError{Digest: value, ActualDigest: actual, Path: path}
	}
	// A clean object has just paid for its own sidecar, so write it: an fsck over a cache DAC wrote before this format should leave it migrated.
	_ = writeMeta(metaPath(path), newMeta(info))
	store.remember(value, info)
	return application.Object{Digest: value, Size: info.Size()}, true, nil
}

// List returns every digest the cache holds, in sorted order.
func (store *Store) List(ctx context.Context) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(store.Root, "blobs", "sha256"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var digests []string
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || strings.HasSuffix(entry.Name(), metaSuffix) {
			continue
		}
		value := digest.Prefix + entry.Name()
		if _, err := digest.Hex(value); err != nil {
			continue
		}
		digests = append(digests, value)
	}
	slices.Sort(digests)
	return digests, nil
}

// Remove deletes one object and its sidecar under the object's digest lock.
func (store *Store) Remove(ctx context.Context, value string) error {
	path, err := store.Path(value)
	if err != nil {
		return err
	}
	return store.WithLock(ctx, value, func() error {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Remove(metaPath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		store.verified.Delete(value)
		return nil
	})
}

// WithLock runs an operation while it holds one digest lock.
func (store *Store) WithLock(ctx context.Context, value string, operation func() error) error {
	hexValue, err := digest.Hex(value)
	if err != nil {
		return err
	}
	path := filepath.Join(store.Root, "locks", hexValue+".lock")
	return flock.Hold(ctx, path, func(context.Context) error { return operation() })
}

// Put installs bytes.
func (store *Store) Put(ctx context.Context, reader io.Reader, options application.PutOptions) (application.Object, error) {
	temporaryDirectory := filepath.Join(store.Root, "tmp")
	temporary, err := atomic.CreateIn(temporaryDirectory, 0o444, atomic.WithTempPrefix("download-"))
	if err != nil {
		return application.Object{}, err
	}
	defer func() { _ = temporary.Discard() }()

	expect := options.Expect
	limit := expect.Size
	if limit == application.Unknown && options.MaxSize > 0 {
		limit = options.MaxSize
	}
	limited := reader
	if limit >= 0 {
		// Read one extra byte so an oversized response cannot look complete.
		limited = io.LimitReader(reader, limit+1)
	}

	hashValue := sha256.New()
	size, err := io.Copy(io.MultiWriter(temporary, hashValue), limited)
	if err != nil {
		return application.Object{}, err
	}
	if err := ctx.Err(); err != nil {
		return application.Object{}, err
	}
	if limit >= 0 && size > limit {
		// An expected size that the bytes overrun is a content failure.
		if expect.Size != application.Unknown {
			return application.Object{}, &application.ContentError{
				ExpectedDigest: expect.Digest,
				ExpectedSize:   expect.Size,
				ActualSize:     application.Unknown,
			}
		}
		return application.Object{}, fmt.Errorf("%w: limit is %d bytes", application.ErrTooLarge, options.MaxSize)
	}
	actualDigest := digest.Prefix + hex.EncodeToString(hashValue.Sum(nil))
	if expect.Size != application.Unknown && size != expect.Size || expect.Digest != "" && actualDigest != expect.Digest {
		return application.Object{}, &application.ContentError{
			ExpectedDigest: expect.Digest,
			ActualDigest:   actualDigest,
			ExpectedSize:   expect.Size,
			ActualSize:     size,
		}
	}
	object := application.Object{Digest: actualDigest, Size: size}

	// Install unconditionally rather than skipping an object that is already there.
	install := func() error {
		path, err := store.Path(actualDigest)
		if err != nil {
			return err
		}
		if err := temporary.CommitAs(path); err != nil {
			return err
		}
		store.trace().Debug("installed", "digest", actualDigest, "size", size)
		return store.record(actualDigest, path)
	}
	if !options.Locked {
		if err := store.WithLock(ctx, actualDigest, install); err != nil {
			return application.Object{}, err
		}
	} else if err := install(); err != nil {
		return application.Object{}, err
	}
	return object, nil
}
