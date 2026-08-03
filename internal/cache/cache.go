// Package cache stores immutable content-addressed objects.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/digest"
	"github.com/tom/dac/internal/proclock"
)

// Store manages one filesystem cache.
type Store struct {
	Root string
}

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

// Stat returns the object stored for a digest without reading its bytes.
func (store *Store) Stat(value string) (application.Object, bool, error) {
	path, err := store.Path(value)
	if err != nil {
		return application.Object{}, false, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return application.Object{}, false, nil
	}
	if err != nil {
		return application.Object{}, false, err
	}
	touch(path)
	return application.Object{Digest: value, Size: info.Size()}, true, nil
}

// touch refreshes an object timestamp so collection can tell an object that is
// still in use from one that no project has referenced in a long time. A
// content-addressed store cannot answer that question on its own, and a
// read-only cache directory is a valid deployment, so a failure is never fatal.
func touch(path string) {
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

// GC removes objects that nothing has used for longer than MaxAge, along with
// temporary files abandoned by an interrupted download.
//
// Age is the only liveness signal a content-addressed store has, which is why
// every cache hit refreshes an object timestamp. Digest lock files are never
// removed: unlinking a lock file that another process holds would let a later
// process take the same lock through a new inode.
func (store *Store) GC(ctx context.Context, options application.GCOptions) (application.GCResult, error) {
	if options.MaxAge < 0 {
		return application.GCResult{}, errors.New("the maximum age must not be negative")
	}
	result := application.GCResult{Digests: []string{}, DryRun: options.DryRun}
	cutoff := time.Now().Add(-options.MaxAge)

	blobs := filepath.Join(store.Root, "blobs", "sha256")
	entries, err := os.ReadDir(blobs)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return application.GCResult{}, err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return application.GCResult{}, err
		}
		if entry.IsDir() {
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
			return application.GCResult{}, err
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		size, removed, err := store.collect(ctx, value, cutoff, options.DryRun)
		if err != nil {
			return application.GCResult{}, err
		}
		if removed {
			result.Digests = append(result.Digests, value)
			result.ObjectCount++
			result.ByteCount += size
		}
	}
	slices.Sort(result.Digests)

	temporary, err := store.collectTemporary(cutoff, options.DryRun)
	if err != nil {
		return application.GCResult{}, err
	}
	result.TempCount = temporary
	return result, nil
}

// collect removes one object while it holds that object's digest lock, so a
// concurrent install cannot rename a new copy into place mid-removal. It
// re-checks the timestamp under the lock because waiting for it takes time
// during which another process may have used the object.
func (store *Store) collect(ctx context.Context, value string, cutoff time.Time, dryRun bool) (int64, bool, error) {
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
		if !info.ModTime().Before(cutoff) {
			return nil
		}
		size, removed = info.Size(), true
		if dryRun {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return size, removed, nil
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

// WithLock runs an operation while it holds one digest lock.
func (store *Store) WithLock(ctx context.Context, value string, operation func() error) error {
	hexValue, err := digest.Hex(value)
	if err != nil {
		return err
	}
	lock, err := proclock.Acquire(ctx, filepath.Join(store.Root, "locks", hexValue+".lock"))
	if err != nil {
		return err
	}
	operationErr := operation()
	return errors.Join(operationErr, lock.Release())
}

// Put installs bytes. Unless the caller already holds the digest lock, it takes
// that lock once it has calculated the digest.
func (store *Store) Put(ctx context.Context, reader io.Reader, options application.PutOptions) (application.Object, error) {
	temporaryDirectory := filepath.Join(store.Root, "tmp")
	if err := os.MkdirAll(temporaryDirectory, 0o755); err != nil {
		return application.Object{}, err
	}
	temporary, err := os.CreateTemp(temporaryDirectory, "download-*")
	if err != nil {
		return application.Object{}, err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()

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
		// An expected size that the bytes overrun is a content failure. A
		// MaxSize overrun is a limit the operator chose, not a broken asset.
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

	install := func() error {
		stored, found, err := store.Stat(actualDigest)
		if err != nil || found && stored.Size == size {
			return err
		}
		path, err := store.Path(actualDigest)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := temporary.Sync(); err != nil {
			return err
		}
		if err := temporary.Chmod(0o444); err != nil {
			return err
		}
		if err := temporary.Close(); err != nil {
			return err
		}
		return os.Rename(temporaryPath, path)
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
