package trust

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/tomdoesdev/dac/internal/jsonfile"
	"github.com/tomdoesdev/dac/internal/proclock"
)

// ResolvePath returns the absolute trusted-hosts file path.
// The trust list is data DAC writes and keeps, so it lives under the XDG data
// location rather than beside the configuration DAC only ever reads, or in the
// cache, which collection is free to empty.
func ResolvePath(option string) (string, error) {
	selected := option
	if selected == "" {
		// Relative XDG paths are ignored so a build cannot trust hosts from its work directory.
		if value := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(value) {
			selected = filepath.Join(value, DirName, FileName)
		}
	}
	if selected == "" {
		home, err := os.UserHomeDir()
		if err != nil || !filepath.IsAbs(home) {
			return "", errors.New("set --trust-file, DAC_TRUST_FILE, XDG_DATA_HOME, or HOME")
		}
		selected = filepath.Join(home, ".local", "share", DirName, FileName)
	}
	return filepath.Abs(selected)
}

// Store reads and writes one trusted-hosts file.
type Store struct {
	Path string
}

// New creates a store without creating its file.
func New(path string) *Store { return &Store{Path: path} }

// Load reads the trusted-hosts file.
// A file that does not exist is an empty list rather than an error: trusting
// nothing yet is the state a first run is in, not a failure to report.
func (store *Store) Load() (List, error) {
	var list List
	if err := jsonfile.ReadStrict(store.Path, &list); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Empty(), nil
		}
		return List{}, err
	}
	if err := list.Normalize(); err != nil {
		return List{}, err
	}
	return list, list.Validate()
}

// Update applies one change to the trusted-hosts file while it holds the file lock.
// Every mutation goes through here, because two DAC processes that each loaded,
// changed, and saved would keep only one of the two changes.
func (store *Store) Update(ctx context.Context, change func(List) (List, error)) (List, error) {
	lock, err := proclock.Acquire(ctx, store.Path+".lock")
	if err != nil {
		return List{}, err
	}
	updated, changeErr := store.write(change)
	return updated, errors.Join(changeErr, lock.Release())
}

// write reads, changes, and replaces the file. Its caller holds the lock.
func (store *Store) write(change func(List) (List, error)) (List, error) {
	list, err := store.Load()
	if err != nil {
		return List{}, err
	}
	updated, err := change(list)
	if err != nil {
		return List{}, err
	}
	if err := updated.Validate(); err != nil {
		return List{}, err
	}
	data, err := json.MarshalIndent(updated, "", "  ")
	if err != nil {
		return List{}, err
	}
	return updated, jsonfile.WriteAtomic(store.Path, append(data, '\n'), fileMode)
}
