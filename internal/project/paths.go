package project

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tomdoesdev/kit/fs/flock"
)

const (
	manifestName = "dac.toml"
	lockName     = "dac.lock"
)

// Paths names every persistent path relative to one discovered project root.
type Paths struct{ Root string }

func (paths Paths) Manifest() string { return filepath.Join(paths.Root, manifestName) }
func (paths Paths) Lockfile() string { return filepath.Join(paths.Root, lockName) }
func (paths Paths) LockPath() string { return flock.HiddenPath(paths.Manifest()) }

// OpenDownloads returns a traversal-safe handle to the managed download
// directory. Opening it through the project root prevents a committed symlink
// from redirecting downloads outside the project.
func (paths Paths) OpenDownloads() (*os.Root, error) {
	root, err := os.OpenRoot(paths.Root)
	if err != nil {
		return nil, NewFilesystemError(err)
	}
	defer func() { _ = root.Close() }()
	managed := filepath.Join(".dac", "downloads")
	if err := root.MkdirAll(managed, 0o755); err != nil {
		return nil, NewFilesystemError(err)
	}
	downloads, err := root.OpenRoot(managed)
	if err != nil {
		return nil, NewFilesystemError(err)
	}
	return downloads, nil
}

// OpenDownloadsOptional opens the managed directory without creating it. This
// keeps observational commands free of persistent filesystem side effects.
func (paths Paths) OpenDownloadsOptional() (*os.Root, bool, error) {
	root, err := os.OpenRoot(paths.Root)
	if err != nil {
		return nil, false, NewFilesystemError(err)
	}
	defer func() { _ = root.Close() }()
	downloads, err := root.OpenRoot(filepath.Join(".dac", "downloads"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, NewFilesystemError(err)
	}
	return downloads, true, nil
}

// Discover walks up from start to the project root. It returns a configuration
// error rather than silently operating in a nearby unrelated directory.
func Discover(start string) (Paths, error) {
	directory, err := filepath.Abs(start)
	if err != nil {
		return Paths{}, NewFilesystemError(err)
	}
	if info, statErr := os.Stat(directory); statErr == nil && !info.IsDir() {
		directory = filepath.Dir(directory)
	}
	for {
		candidate := filepath.Join(directory, manifestName)
		if _, err := os.Stat(candidate); err == nil {
			return Paths{Root: directory}, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Paths{}, NewFilesystemError(err)
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return Paths{}, NewConfigurationError(ErrProjectNotFound, WithHint("run `dac init`"))
		}
		directory = parent
	}
}

// WithLock serializes complete command transitions. The hidden sidecar is
// removed after release, so it never becomes project state users must manage.
func (paths Paths) WithLock(ctx context.Context, work func(context.Context) error) error {
	err := flock.Hold(ctx, paths.LockPath(), work, flock.RemoveOnRelease())
	if errors.Is(err, errors.ErrUnsupported) {
		return NewUnsupportedError(ErrFlockUnsupported)
	}
	var operation *Error
	if errors.As(err, &operation) {
		// Preserve the command's category after the lock releases so callers see
		// the actual configuration, integrity, or network failure.
		return err
	}
	if errors.Is(err, context.Canceled) {
		return NewCancelledError(err)
	}
	if err != nil {
		return NewFilesystemError(err)
	}
	return nil
}
