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
// A larger operation that may need to undo several installed files can use
// CommitReversible or CommitNoReplace. Both return a Commit whose Rollback
// undoes that installation and whose Complete releases its recovery state.
//
// A replacing commit renames, while a no-replace commit creates a hard link.
// Both require the destination and temporary file to be on the same file
// system. The destination takes the mode passed to Create rather than keeping
// the mode of a file it replaces, and a symbolic-link destination is replaced
// by a regular file rather than followed.
//
// This package targets Unix. It builds elsewhere, but off Unix a commit cannot
// sync the directory it renamed into, so the rename is atomic without being
// durable.
package atomic

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tomdoesdev/kit/fs/flock"
)

var (
	// ErrNoDestination marks a Commit that has no path to commit to.
	// This is a mistake in the calling code rather than a state to recover from.
	ErrNoDestination = errors.New("this file has no destination: name one with CommitAs")
	// ErrNilRoot marks rooted staging without an open filesystem root.
	ErrNilRoot = errors.New("atomic: root must not be nil")
	// ErrDestinationIsDirectory marks a destination that cannot be moved aside.
	ErrDestinationIsDirectory = errors.New("destination is a directory")
	// ErrInvalidTempPattern marks rooted temporary prefixes that contain a separator.
	ErrInvalidTempPattern = errors.New("pattern contains path separator")
)

// File is one write in progress.
// It is not safe for concurrent use, in the way that os.File is not.
type File struct {
	file        *os.File
	temp        string
	destination string
	perm        fs.FileMode
	settings    settings
	filesystem  filesystem
	done        bool
}

// Commit is one installed file that can still be rolled back.
// Complete keeps the destination and removes recovery state. Rollback removes
// the installed file and restores the destination that a reversible commit
// moved aside, when there was one.
type Commit struct {
	destination string
	backup      string
	temporary   string
	sync        bool
	filesystem  filesystem
	installed   bool
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

// WriteFileLocked replaces path while holding its hidden sidecar lock. The
// lock covers one write only; callers that read, change, and write must hold a
// lock around their entire operation instead.
func WriteFileLocked(path string, data []byte, perm fs.FileMode, options ...Option) error {
	return flock.Hold(context.Background(), flock.HiddenPath(path), func(context.Context) error {
		return WriteFile(path, data, perm, options...)
	}, flock.RemoveOnRelease())
}

// Create opens a temporary file beside path that Commit renames onto path.
// It creates the directory of path unless WithoutMkdir says not to.
func Create(path string, perm fs.FileMode, options ...Option) (*File, error) {
	file, err := createIn(pathFilesystem{}, filepath.Dir(path), perm, options...)
	if err != nil {
		return nil, err
	}
	file.destination = path
	return file, nil
}

// CreateRoot opens a temporary file inside root that Commit renames onto path.
// Every filesystem operation remains confined to root, including when a path
// component is a symbolic link. Root must remain open until the File has been
// committed or discarded and any reversible Commit has completed or rolled back.
func CreateRoot(root *os.Root, path string, perm fs.FileMode, options ...Option) (*File, error) {
	if root == nil {
		return nil, ErrNilRoot
	}
	file, err := createIn(rootFilesystem{root: root}, filepath.Dir(path), perm, options...)
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
	return createIn(pathFilesystem{}, dir, perm, options...)
}

// createIn contains the common staging behavior for ordinary paths and roots.
func createIn(filesystem filesystem, dir string, perm fs.FileMode, options ...Option) (*File, error) {
	current := newSettings(options)
	if err := makeDir(filesystem, dir, current); err != nil {
		return nil, err
	}
	temporary, temporaryPath, err := filesystem.createTemp(dir, current.tempPrefix)
	if err != nil {
		return nil, err
	}
	return &File{file: temporary, temp: temporaryPath, perm: perm, settings: current, filesystem: filesystem}, nil
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
		return ErrNoDestination
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

// CommitReversible replaces the destination and returns a handle that can put
// its previous entry back. Complete must be called once rollback is no longer
// needed so the previous entry can be removed.
//
// A non-nil Commit may accompany an error after filesystem state changed. The
// caller must retain and Rollback that Commit on the error path.
func (file *File) CommitReversible() (*Commit, error) {
	if file.done {
		return nil, nil
	}
	if file.destination == "" {
		return nil, ErrNoDestination
	}
	return file.commitReversible(file.destination)
}

// CommitAsReversible is CommitReversible for a destination chosen after CreateIn.
func (file *File) CommitAsReversible(path string) (*Commit, error) {
	if file.done {
		return nil, nil
	}
	return file.commitReversible(path)
}

// CommitNoReplace installs the file only when the destination does not exist.
// It returns a handle that can remove the installed destination during a
// larger operation's rollback. An existing destination causes an error that
// matches fs.ErrExist and remains untouched.
//
// A non-nil Commit may accompany an error after filesystem state changed. The
// caller must retain and Rollback that Commit on the error path.
func (file *File) CommitNoReplace() (*Commit, error) {
	if file.done {
		return nil, nil
	}
	if file.destination == "" {
		return nil, ErrNoDestination
	}
	return file.commitNoReplace(file.destination)
}

// CommitAsNoReplace is CommitNoReplace for a destination chosen after CreateIn.
func (file *File) CommitAsNoReplace(path string) (*Commit, error) {
	if file.done {
		return nil, nil
	}
	return file.commitNoReplace(path)
}

// Discard removes the temporary file. Discard is idempotent, and it does
// nothing after a commit that succeeded, so it is safe to defer in every case.
//
// A commit that fails without returning a Commit leaves the temporary file to
// discard. A Commit returned alongside an error owns the recovery names
// instead, and the caller must roll it back.
func (file *File) Discard() error {
	if file.done {
		return nil
	}
	file.done = true
	closeErr := file.file.Close()
	if errors.Is(closeErr, fs.ErrClosed) {
		closeErr = nil
	}
	removeErr := file.filesystem.remove(file.temp)
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}

// Complete keeps the installed destination and removes any previous entry or
// temporary name retained for rollback. Complete is idempotent.
func (commit *Commit) Complete() error {
	if commit == nil || commit.done {
		return nil
	}
	// A reversible commit can fail after moving the old destination aside but
	// before installing the new one. Completing that state must restore rather
	// than delete the only remaining copy of the previous destination.
	if !commit.installed && commit.backup != "" {
		return commit.Rollback()
	}
	commit.done = true
	var cleanup []error
	changed := false
	if commit.backup != "" {
		cleanup = append(cleanup, commit.filesystem.remove(commit.backup))
		changed = true
	}
	if commit.temporary != "" {
		cleanup = append(cleanup, commit.filesystem.remove(commit.temporary))
		changed = true
	}
	if commit.sync && changed {
		cleanup = append(cleanup, commit.filesystem.syncDir(filepath.Dir(commit.destination)))
	}
	return errors.Join(cleanup...)
}

// Rollback removes the installed destination and restores the entry that a
// reversible commit replaced. Rollback is idempotent.
func (commit *Commit) Rollback() error {
	if commit == nil || commit.done {
		return nil
	}
	commit.done = true
	var rollback []error
	changed := false
	if commit.installed {
		rollback = append(rollback, commit.filesystem.remove(commit.destination))
		changed = true
	}
	if commit.backup != "" {
		rollback = append(rollback, commit.filesystem.rename(commit.backup, commit.destination))
		changed = true
	}
	if commit.temporary != "" {
		rollback = append(rollback, commit.filesystem.remove(commit.temporary))
		changed = true
	}
	if commit.sync && changed {
		rollback = append(rollback, commit.filesystem.syncDir(filepath.Dir(commit.destination)))
	}
	return errors.Join(rollback...)
}

// commit flushes the file, sets its mode, and renames it onto path.
func (file *File) commit(path string) error {
	directory, err := file.prepare(path)
	if err != nil {
		return err
	}
	if err := file.filesystem.rename(file.temp, path); err != nil {
		return err
	}
	// The temporary name is gone from here, so there is nothing left to
	// discard even when the directory sync reports a failure.
	file.done = true
	if !file.settings.sync {
		return nil
	}
	return file.filesystem.syncDir(directory)
}

// commitReversible moves the current destination aside before installing the
// staged file. The returned handle owns every name needed to undo that change.
func (file *File) commitReversible(path string) (*Commit, error) {
	directory, err := file.prepare(path)
	if err != nil {
		return nil, err
	}
	commit := &Commit{destination: path, temporary: file.temp, sync: file.settings.sync, filesystem: file.filesystem}
	backup, err := moveAside(file.filesystem, path, file.settings.tempPrefix+"backup-")
	if err != nil {
		return nil, err
	}
	commit.backup = backup
	// The Commit owns both recovery names from here, including when the final
	// rename fails and the caller has to restore the backup.
	file.done = true
	if err := file.filesystem.rename(file.temp, path); err != nil {
		return commit, err
	}
	commit.installed = true
	commit.temporary = ""
	if !file.settings.sync {
		return commit, nil
	}
	return commit, file.filesystem.syncDir(directory)
}

// commitNoReplace uses a hard link as the atomic existence check. The staged
// name is removed only after the destination points at the complete inode.
func (file *File) commitNoReplace(path string) (*Commit, error) {
	directory, err := file.prepare(path)
	if err != nil {
		return nil, err
	}
	if err := file.filesystem.link(file.temp, path); err != nil {
		return nil, err
	}
	file.done = true
	commit := &Commit{
		destination: path,
		temporary:   file.temp,
		sync:        file.settings.sync,
		filesystem:  file.filesystem,
		installed:   true,
	}
	removeErr := file.filesystem.remove(file.temp)
	if removeErr == nil {
		commit.temporary = ""
	}
	if !file.settings.sync {
		return commit, removeErr
	}
	return commit, errors.Join(removeErr, file.filesystem.syncDir(directory))
}

// prepare makes the destination directory and seals the staged inode before
// any operation changes a destination name.
func (file *File) prepare(path string) (string, error) {
	directory := filepath.Dir(path)
	// The directory comes first. A destination that cannot be made is cheap to
	// find out about, and an fsync of a file that may be gigabytes is not.
	if err := makeDir(file.filesystem, directory, file.settings); err != nil {
		return "", err
	}
	if file.settings.sync {
		if err := file.file.Sync(); err != nil {
			return "", err
		}
	}
	// The mode is set here rather than at Create, so that the file keeps the
	// 0600 that os.CreateTemp gives it for the whole time it is writable.
	if err := file.file.Chmod(file.perm); err != nil {
		return "", err
	}
	if err := file.file.Close(); err != nil {
		return "", err
	}
	return directory, nil
}

// moveAside gives an existing non-directory entry a unique name beside its
// destination. A missing destination needs no recovery name.
func moveAside(filesystem filesystem, path, prefix string) (string, error) {
	info, err := filesystem.lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", &fs.PathError{Op: "rename", Path: path, Err: ErrDestinationIsDirectory}
	}
	reserved, backup, err := filesystem.createTemp(filepath.Dir(path), prefix)
	if err != nil {
		return "", err
	}
	if err := reserved.Close(); err != nil {
		_ = filesystem.remove(backup)
		return "", err
	}
	if err := filesystem.remove(backup); err != nil {
		return "", err
	}
	if err := filesystem.rename(path, backup); err != nil {
		return "", err
	}
	return backup, nil
}

// makeDir creates a directory and its parents unless the options forbid it.
func makeDir(filesystem filesystem, path string, current settings) error {
	if !current.makeDirs {
		return nil
	}
	return filesystem.mkdirAll(path, current.dirMode)
}
