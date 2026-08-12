package atomic

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tomdoesdev/kit/fs/atomic/internal/sys"
)

// filesystem is the narrow set of path operations an atomic transaction uses.
// Keeping ordinary and rooted paths behind this seam gives them identical
// commit behavior while os.Root enforces project containment for callers that
// process paths from untrusted configuration.
type filesystem interface {
	createTemp(directory, prefix string) (*os.File, string, error)
	lstat(path string) (fs.FileInfo, error)
	link(oldPath, newPath string) error
	mkdirAll(path string, perm fs.FileMode) error
	remove(path string) error
	rename(oldPath, newPath string) error
	syncDir(path string) error
}

type pathFilesystem struct{}

func (pathFilesystem) createTemp(directory, prefix string) (*os.File, string, error) {
	file, err := os.CreateTemp(directory, prefix+"*")
	if err != nil {
		return nil, "", err
	}
	return file, file.Name(), nil
}
func (pathFilesystem) lstat(path string) (fs.FileInfo, error) { return os.Lstat(path) }
func (pathFilesystem) link(oldPath, newPath string) error     { return os.Link(oldPath, newPath) }
func (pathFilesystem) mkdirAll(path string, perm fs.FileMode) error {
	return os.MkdirAll(path, perm)
}
func (pathFilesystem) remove(path string) error { return removeMissingOK(os.Remove(path)) }
func (pathFilesystem) rename(oldPath, newPath string) error {
	return os.Rename(oldPath, newPath)
}
func (pathFilesystem) syncDir(path string) error { return sys.SyncDir(path) }

type rootFilesystem struct{ root *os.Root }

func (filesystem rootFilesystem) createTemp(directory, prefix string) (*os.File, string, error) {
	if containsPathSeparator(prefix) {
		return nil, "", &fs.PathError{Op: "createtemp", Path: prefix, Err: errors.New("pattern contains path separator")}
	}
	for range 100 {
		suffix := make([]byte, 16)
		if _, err := rand.Read(suffix); err != nil {
			return nil, "", err
		}
		name := filepath.Join(directory, prefix+hex.EncodeToString(suffix))
		file, err := filesystem.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return file, name, nil
	}
	return nil, "", &fs.PathError{Op: "createtemp", Path: directory, Err: fs.ErrExist}
}
func (filesystem rootFilesystem) lstat(path string) (fs.FileInfo, error) {
	return filesystem.root.Lstat(path)
}
func (filesystem rootFilesystem) link(oldPath, newPath string) error {
	return filesystem.root.Link(oldPath, newPath)
}
func (filesystem rootFilesystem) mkdirAll(path string, perm fs.FileMode) error {
	return filesystem.root.MkdirAll(path, perm)
}
func (filesystem rootFilesystem) remove(path string) error {
	return removeMissingOK(filesystem.root.Remove(path))
}
func (filesystem rootFilesystem) rename(oldPath, newPath string) error {
	return filesystem.root.Rename(oldPath, newPath)
}
func (filesystem rootFilesystem) syncDir(path string) error {
	return sys.SyncRootDir(filesystem.root, path)
}

// removeMissingOK makes cleanup idempotent across retries and rollback paths.
func removeMissingOK(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// containsPathSeparator enforces the same flat prefix contract as os.CreateTemp.
func containsPathSeparator(value string) bool {
	for index := range len(value) {
		if os.IsPathSeparator(value[index]) {
			return true
		}
	}
	return false
}
