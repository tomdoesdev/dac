package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/tomdoesdev/kit/fs/atomic"
	"github.com/tomdoesdev/kit/fs/flock"
	"github.com/tomdoesdev/kit/strictjson"
)

// Path returns the catalog file for one cache root.
//
// The catalog sits at the root of the cache rather than among the objects, because collection
// walks the object and staging directories and nothing else: a file at the root is out of its
// reach. Keeping it there means a cache that is moved, copied, or shared carries the record of
// what it holds with it, instead of leaving that record behind on the machine that wrote it.
//
// A catalog therefore belongs to one cache root. A run pointed at another root by --cache-dir
// reads that root's record, which is the record describing the objects it can answer for.
func Path(cacheRoot string) string {
	return filepath.Join(cacheRoot, FileName)
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
	if err := strictjson.ReadFile(store.Path, &catalog); err != nil {
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
