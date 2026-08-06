// Package atomic replaces a file in one step.
//
// A write goes to a temporary file beside the destination, and a rename puts
// it in place. A reader therefore sees the old file or the new one, never a
// half-written one, and a write that fails leaves the old file alone.
//
// WriteFile does this for a slice of bytes. Create returns a handle for
// content that arrives over time, and the handle commits or discards:
//
//	file, err := atomic.Create(path, 0o644)
//	if err != nil {
//		return err
//	}
//	defer func() { _ = file.Discard() }()
//	if _, err := io.Copy(file, source); err != nil {
//		return err
//	}
//	return file.Commit()
//
// Discard after a commit that succeeded does nothing, so the deferred call
// above is the only cleanup the caller needs on any path out.
//
// A commit renames, which has three consequences worth knowing. The
// destination must be on the same file system as the temporary file, or the
// rename fails with EXDEV. The destination takes the mode passed to Create,
// rather than keeping the mode of the file it replaces. And a destination that
// is a symbolic link is replaced by a regular file rather than followed.
//
// This package targets Unix. It builds elsewhere, but off Unix a commit cannot
// sync the directory it renamed into, so the rename is atomic without being
// durable.
package atomic

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tomdoesdev/dac/internal/fs/atomic/internal/sys"
)

// errNoDestination reports a Commit that has no path to commit to.
// This is a mistake in the calling code rather than a state to recover from.
var errNoDestination = errors.New("this file has no destination: name one with CommitAs")

// File is one write in progress.
// It is not safe for concurrent use, in the way that os.File is not.
type File struct {
	file        *os.File
	temp        string
	destination string
	perm        fs.FileMode
	settings    settings
	done        bool
}

// WriteFile writes data to path, and replaces what is there already.
//
// The mode is exact. Unlike os.WriteFile, a umask does not narrow it, because
// the mode reaches the temporary file through fchmod(2).
func WriteFile(path string, data []byte, perm fs.FileMode, options ...Option) error {
	file, err := Create(path, perm, options...)
	if err != nil {
		return err
	}
	defer func() { _ = file.Discard() }()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Commit()
}

// Create opens a temporary file beside path that Commit renames onto path.
// It creates the directory of path unless WithoutMkdir says not to.
func Create(path string, perm fs.FileMode, options ...Option) (*File, error) {
	file, err := CreateIn(filepath.Dir(path), perm, options...)
	if err != nil {
		return nil, err
	}
	file.destination = path
	return file, nil
}

// CreateIn opens a temporary file in dir, and leaves the destination open.
// The caller names it with CommitAs, which is the way to write content whose
// destination depends on the content, such as a name that holds its digest.
// Commit without CommitAs fails, because there is nowhere to commit to.
func CreateIn(dir string, perm fs.FileMode, options ...Option) (*File, error) {
	current := newSettings(options)
	if err := makeDir(dir, current); err != nil {
		return nil, err
	}
	// os.CreateTemp already reports the directory it could not write in.
	temporary, err := os.CreateTemp(dir, current.tempPrefix+"*")
	if err != nil {
		return nil, err
	}
	return &File{file: temporary, temp: temporary.Name(), perm: perm, settings: current}, nil
}

// Write writes to the temporary file.
func (file *File) Write(data []byte) (int, error) {
	return file.file.Write(data)
}

// ReadFrom copies the reader into the temporary file.
// It is here so that io.Copy reaches the copy the kernel can make, rather than
// moving every byte through a buffer of its own.
func (file *File) ReadFrom(reader io.Reader) (int64, error) {
	return file.file.ReadFrom(reader)
}

// Path reports the temporary path, which exists until a commit or a discard.
func (file *File) Path() string { return file.temp }

// Commit puts the file at the path that Create was given.
// A file from CreateIn has no such path, and fails.
func (file *File) Commit() error {
	if file.done {
		return nil
	}
	if file.destination == "" {
		return errNoDestination
	}
	return file.commit(file.destination)
}

// CommitAs puts the file at path, and replaces what is there already.
// The path must be on the same file system as the temporary file.
func (file *File) CommitAs(path string) error {
	if file.done {
		return nil
	}
	return file.commit(path)
}

// Discard removes the temporary file. Discard is idempotent, and it does
// nothing after a commit that succeeded, so it is safe to defer in every case.
//
// A commit that failed leaves the temporary file to discard, because a
// destination that was never written is a destination the caller still owns.
func (file *File) Discard() error {
	if file.done {
		return nil
	}
	file.done = true
	closeErr := file.file.Close()
	if errors.Is(closeErr, fs.ErrClosed) {
		closeErr = nil
	}
	removeErr := os.Remove(file.temp)
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}

// commit flushes the file, sets its mode, and renames it onto path.
func (file *File) commit(path string) error {
	directory := filepath.Dir(path)
	// The directory comes first. A destination that cannot be made is cheap to
	// find out about, and an fsync of a file that may be gigabytes is not.
	if err := makeDir(directory, file.settings); err != nil {
		return err
	}
	if file.settings.sync {
		if err := file.file.Sync(); err != nil {
			return err
		}
	}
	// The mode is set here rather than at Create, so that the file keeps the
	// 0600 that os.CreateTemp gives it for the whole time it is writable.
	if err := file.file.Chmod(file.perm); err != nil {
		return err
	}
	if err := file.file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.temp, path); err != nil {
		return err
	}
	// The temporary name is gone from here, so there is nothing left to
	// discard even when the directory sync reports a failure.
	file.done = true
	if !file.settings.sync {
		return nil
	}
	return sys.SyncDir(directory)
}

// makeDir creates a directory and its parents unless the options forbid it.
func makeDir(path string, current settings) error {
	if !current.makeDirs {
		return nil
	}
	return os.MkdirAll(path, current.dirMode)
}
