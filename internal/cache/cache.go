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
		return store.deleteObject(value, path)
	})
}

// deleteObject removes an object and its integrity sidecar. Callers hold the
// digest lock so installation cannot replace either file midway through removal.
func (store *Store) deleteObject(value, path string) error {
	store.verified.Delete(value)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(metaPath(path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
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
