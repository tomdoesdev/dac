package project

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/tomdoesdev/dac/internal/fault"
)

// downloadsDir is the managed directory, relative to the project root, that
// holds every file dac has accepted.
var downloadsDir = filepath.Join(".dac", "downloads")

// Downloads is a traversal-safe handle to the managed download directory and
// the operations dac performs on it as a whole. Opening it through the project
// root prevents a committed symlink from redirecting downloads outside the
// project.
type Downloads struct{ root *os.Root }

// Root exposes the underlying directory for callers that read or write one
// file at a time, such as staging and verifying an individual artifact.
func (downloads *Downloads) Root() *os.Root { return downloads.root }

// Close releases the directory handle.
func (downloads *Downloads) Close() error { return downloads.root.Close() }

// OpenDownloads returns the managed directory, creating it when absent.
func (paths Paths) OpenDownloads() (*Downloads, error) {
	downloads, _, err := paths.openDownloads(true)
	return downloads, err
}

// OpenDownloadsOptional returns the managed directory without creating it, and
// reports whether it exists. This keeps observational commands free of
// persistent filesystem side effects.
func (paths Paths) OpenDownloadsOptional() (*Downloads, bool, error) {
	return paths.openDownloads(false)
}

func (paths Paths) openDownloads(create bool) (*Downloads, bool, error) {
	root, err := os.OpenRoot(paths.Root)
	if err != nil {
		return nil, false, fault.NewFilesystemError(err)
	}
	defer func() { _ = root.Close() }()
	if create {
		if err := root.MkdirAll(downloadsDir, 0o755); err != nil {
			return nil, false, fault.NewFilesystemError(err)
		}
	}
	managed, err := root.OpenRoot(downloadsDir)
	if errors.Is(err, fs.ErrNotExist) && !create {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fault.NewFilesystemError(err)
	}
	return &Downloads{root: managed}, true, nil
}

// Prune removes files the previous accepted state referenced that the next one
// does not. It reports what it could not remove rather than failing, because a
// stale file is untidy but never makes the new state wrong. Entries dac never
// tracked are left alone; status reports those separately.
func (downloads *Downloads) Prune(previous, retained []string) []string {
	keep := make(map[string]bool, len(retained))
	for _, name := range retained {
		keep[name] = true
	}
	var warnings []string
	removed := false
	seen := make(map[string]bool, len(previous))
	for _, name := range previous {
		if keep[name] || seen[name] {
			continue
		}
		seen[name] = true
		info, err := downloads.root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			warnings = append(warnings, "could not inspect obsolete download "+strconv.Quote(name)+": "+err.Error())
			continue
		}
		if info.IsDir() {
			warnings = append(warnings, "obsolete download is a directory and was not removed: "+strconv.Quote(name))
			continue
		}
		if err := downloads.root.Remove(name); err != nil {
			warnings = append(warnings, "could not remove obsolete download "+strconv.Quote(name)+": "+err.Error())
			continue
		}
		removed = true
	}
	if removed {
		if err := downloads.sync(); err != nil {
			warnings = append(warnings, "could not sync downloads after removing obsolete files: "+err.Error())
		}
	}
	return warnings
}

// Unreferenced lists directory entries that no accepted file claims, so a
// caller can report them without deciding they are safe to delete.
func (downloads *Downloads) Unreferenced(referenced []string) ([]string, error) {
	claimed := make(map[string]bool, len(referenced))
	for _, name := range referenced {
		claimed[name] = true
	}
	entries, err := fs.ReadDir(downloads.root.FS(), ".")
	if err != nil {
		return nil, fault.NewFilesystemError(err)
	}
	var result []string
	for _, entry := range entries {
		if !claimed[entry.Name()] {
			result = append(result, entry.Name())
		}
	}
	return result, nil
}

// sync durably records directory changes so a crash cannot resurrect a file
// dac has already reported as removed.
func (downloads *Downloads) sync() error {
	directory, err := downloads.root.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
