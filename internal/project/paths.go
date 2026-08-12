package project

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tomdoesdev/dac/internal/fault"
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

// Discover walks up from start to the project root. It returns a configuration
// error rather than silently operating in a nearby unrelated directory.
func Discover(start string) (Paths, error) {
	directory, err := filepath.Abs(start)
	if err != nil {
		return Paths{}, fault.NewFilesystemError(err)
	}
	if info, statErr := os.Stat(directory); statErr == nil && !info.IsDir() {
		directory = filepath.Dir(directory)
	}
	for {
		candidate := filepath.Join(directory, manifestName)
		if _, err := os.Stat(candidate); err == nil {
			return Paths{Root: directory}, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Paths{}, fault.NewFilesystemError(err)
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return Paths{}, fault.NewConfigurationError(ErrProjectNotFound, fault.WithRecovery(fault.Recovery{Command: "init"}))
		}
		directory = parent
	}
}

// WithLock serializes complete command transitions. The hidden sidecar is
// removed after release, so it never becomes project state users must manage.
func (paths Paths) WithLock(ctx context.Context, work func(context.Context) error) error {
	err := flock.Hold(ctx, paths.LockPath(), work, flock.RemoveOnRelease())
	if errors.Is(err, errors.ErrUnsupported) {
		return fault.NewUnsupportedError(ErrFlockUnsupported)
	}
	var operation *fault.Error
	if errors.As(err, &operation) {
		// Preserve the command's category after the lock releases so callers see
		// the actual configuration, integrity, or network failure.
		return err
	}
	if errors.Is(err, context.Canceled) {
		return fault.NewCancelledError(err)
	}
	if err != nil {
		return fault.NewFilesystemError(err)
	}
	return nil
}
