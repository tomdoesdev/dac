package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/tomdoesdev/dac/internal/fs/atomic"
	"github.com/tomdoesdev/dac/internal/fs/flock"
	"github.com/tomdoesdev/dac/internal/jsonfile"
)

// ResolvePath returns the absolute catalog file path.
// The catalog sits with the trusted-hosts file under the XDG data location, because it is data
// DAC writes and means to keep. It is deliberately not in the cache: a record that lived where
// the objects live would be emptied by the same commands that empty them, and a record of what
// DAC has downloaded is worth more than that.
//
// One catalog therefore serves every cache root. A run pointed at another root by --cache-dir
// reads records for objects that root does not hold, which costs a listing nothing, because a
// listing only reports objects the cache answers for.
func ResolvePath(option string) (string, error) {
	selected := option
	if selected == "" {
		// Relative XDG paths are ignored, matching the trusted-hosts file.
		if value := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(value) {
			selected = filepath.Join(value, DirName, FileName)
		}
	}
	if selected == "" {
		home, err := os.UserHomeDir()
		if err != nil || !filepath.IsAbs(home) {
			return "", errors.New("set XDG_DATA_HOME or HOME")
		}
		selected = filepath.Join(home, ".local", "share", DirName, FileName)
	}
	return filepath.Abs(selected)
}

// Store reads and writes one catalog file.
type Store struct {
	Path string
}

// New creates a store without creating its file.
func New(path string) *Store { return &Store{Path: path} }

// Load reads the catalog file.
// A file that does not exist is an empty catalog rather than an error: recording nothing yet is
// the state a first run is in, not a failure to report.
//
// A file that cannot be read is reported, and the caller decides. It is not quietly replaced with
// an empty one here, because the next write would then destroy every record the file held, and
// unlike the object metadata in the cache, nothing regenerates those.
func (store *Store) Load() (Catalog, error) {
	var catalog Catalog
	if err := jsonfile.ReadStrict(store.Path, &catalog); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Empty(), nil
		}
		return Catalog{}, err
	}
	return catalog, catalog.Validate()
}

// Update applies one change to the catalog file while it holds the file lock.
// Every mutation goes through here, because two DAC processes that each loaded, changed, and
// saved would keep only one of the two changes.
func (store *Store) Update(ctx context.Context, change func(Catalog) (Catalog, error)) (Catalog, error) {
	var updated Catalog
	err := flock.Hold(ctx, flock.HiddenPath(store.Path), func(context.Context) error {
		var changeErr error
		updated, changeErr = store.write(change)
		return changeErr
	}, flock.RemoveOnRelease())
	if err != nil {
		return Catalog{}, err
	}
	return updated, nil
}

// write reads, changes, and replaces the file. Its caller holds the lock.
// A change that produced the bytes already on disk writes nothing, because every command that
// reads a project reaches here, and most of them have nothing new to say.
func (store *Store) write(change func(Catalog) (Catalog, error)) (Catalog, error) {
	catalog, err := store.Load()
	if err != nil {
		return Catalog{}, err
	}
	updated, err := change(catalog)
	if err != nil {
		return Catalog{}, err
	}
	if err := updated.Validate(); err != nil {
		return Catalog{}, err
	}
	data, err := json.MarshalIndent(updated, "", "  ")
	if err != nil {
		return Catalog{}, err
	}
	data = append(data, '\n')
	existing, err := os.ReadFile(store.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Catalog{}, err
	}
	if bytes.Equal(existing, data) {
		return updated, nil
	}
	return updated, atomic.WriteFile(store.Path, data, fileMode)
}
