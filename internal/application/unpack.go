package application

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

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/kit/fs/atomic"
	"github.com/tomdoesdev/kit/fs/util/filename"
)

// UnpackOptions selects cached assets and their destination.
type UnpackOptions struct {
	Directory string
	Assets    []Selection
	Force     bool
}

// UnpackedFile reports one materialized cache object.
type UnpackedFile struct {
	Coordinate string `json:"coordinate"`
	Filename   string `json:"filename"`
	Path       string `json:"path"`
	Digest     string `json:"digest"`
	Size       int64  `json:"size"`
}

// UnpackResult reports the files written from the project cache.
type UnpackResult struct {
	Directory    string         `json:"directory"`
	ProjectCount int            `json:"projectCount"`
	FileCount    int            `json:"fileCount"`
	ByteCount    int64          `json:"byteCount"`
	Files        []UnpackedFile `json:"files"`
}

type unpackTarget struct {
	coordinate  coord.Coordinate
	filename    string
	source      string
	destination string
	object      Object
	staged      *atomic.File
	commit      *atomic.Commit
}

const (
	unpackTempPrefix = ".dac-unpack-"
	// copyBufferSize is the buffer each staged file is copied through, in place of io.Copy's 32 KiB default.
	copyBufferSize = 1 << 20
)

// Unpack writes selected cached assets under their friendly file names.
func (service *Service) Unpack(ctx context.Context, options UnpackOptions) (UnpackResult, error) {
	directory, err := filepath.Abs(options.Directory)
	if err != nil {
		return UnpackResult{}, fault.Wrap("unpack_write_failed", "DAC could not resolve the destination directory.", err)
	}
	manifest, lock, err := service.readProject()
	if err != nil {
		return UnpackResult{}, err
	}
	names, err := chosen(manifest.Assets, options.Assets)
	if err != nil {
		return UnpackResult{}, err
	}
	targets, err := unpackTargets(directory, names, manifest, lock)
	if err != nil {
		return UnpackResult{}, err
	}
	if err := checkUnpackNames(targets); err != nil {
		return UnpackResult{}, err
	}
	missingDirectories, err := inspectUnpackDirectory(directory)
	if err != nil {
		return UnpackResult{}, err
	}
	if err := checkUnpackDestinations(targets, options.Force); err != nil {
		return UnpackResult{}, err
	}
	if err := service.resolveUnpackSources(targets); err != nil {
		return UnpackResult{}, err
	}
	createdDirectories, err := createUnpackDirectories(missingDirectories)
	if err != nil {
		return UnpackResult{}, fault.Wrap("unpack_write_failed", "DAC could not create the destination directory.", err)
	}
	if err := stageUnpack(ctx, targets); err != nil {
		removeUnpackDirectories(createdDirectories)
		return UnpackResult{}, err
	}
	if err := ctx.Err(); err != nil {
		cleanupUnpack(targets)
		removeUnpackDirectories(createdDirectories)
		return UnpackResult{}, fault.Wrap("cancelled", "The command was cancelled.", err)
	}
	if err := commitUnpack(targets, options.Force); err != nil {
		removeUnpackDirectories(createdDirectories)
		return UnpackResult{}, err
	}

	result := UnpackResult{
		Directory:    directory,
		ProjectCount: len(manifest.Assets),
		FileCount:    len(targets),
		Files:        make([]UnpackedFile, 0, len(targets)),
	}
	for _, target := range targets {
		result.ByteCount += target.object.Size
		result.Files = append(result.Files, UnpackedFile{
			Coordinate: target.coordinate.String(),
			Filename:   target.filename,
			Path:       target.destination,
			Digest:     target.object.Digest,
			Size:       target.object.Size,
		})
	}
	return result, nil
}

// inspectUnpackDirectory finds missing path elements and rejects a symlink or file at the destination boundary.
func inspectUnpackDirectory(directory string) ([]string, error) {
	var missing []string
	current := directory
	for {
		info, err := os.Lstat(current)
		switch {
		case err == nil && info.IsDir():
			return missing, nil
		case err == nil:
			return nil, &fault.Error{
				Code:    "unpack_destination_occupied",
				Message: "The destination directory is not a real directory. DAC will not follow or replace it.",
				Details: map[string]any{"files": []string{current}},
			}
		case !errors.Is(err, os.ErrNotExist):
			return nil, fault.Wrap("unpack_write_failed", "DAC could not check the destination directory.", err)
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			return nil, fault.New("unpack_write_failed", "DAC could not find an existing destination parent directory.")
		}
		current = parent
	}
}

// createUnpackDirectories creates each missing path element without following a new symlink.
func createUnpackDirectories(missing []string) ([]string, error) {
	created := make([]string, 0, len(missing))
	for index := len(missing) - 1; index >= 0; index-- {
		if err := os.Mkdir(missing[index], 0o755); err != nil {
			removeUnpackDirectories(created)
			return nil, err
		}
		created = append(created, missing[index])
	}
	return created, nil
}

// removeUnpackDirectories removes empty directories that a failed unpack created.
func removeUnpackDirectories(created []string) {
	for index := len(created) - 1; index >= 0; index-- {
		_ = os.Remove(created[index])
	}
}

// unpackTargets resolves all names before DAC checks or writes cache objects.
func unpackTargets(directory string, names []coord.Coordinate, manifest project.Manifest, lock project.Lock) ([]*unpackTarget, error) {
	targets := make([]*unpackTarget, 0, len(names))
	for _, name := range names {
		source := manifest.Assets[name]
		locked := lock.Assets[name]
		friendly, err := unpackFilename(name, source, locked)
		if err != nil {
			return nil, withAsset(fault.Wrap("lock_invalid", "The asset has an invalid file name.", err), name.String())
		}
		targets = append(targets, &unpackTarget{
			coordinate:  name,
			filename:    friendly,
			destination: filepath.Join(directory, friendly),
			object:      Object{Digest: locked.Digest, Size: locked.Size},
		})
	}
	return targets, nil
}

// resolveUnpackSources confirms that every selected object is usable before staging.
func (service *Service) resolveUnpackSources(targets []*unpackTarget) error {
	for _, target := range targets {
		valid, err := service.cached(target.object)
		if err != nil {
			return withAsset(unpackCacheError(err), target.coordinate.String())
		}
		if !valid {
			return withAsset(fault.New("cache_object_invalid", "The cache object is missing. Run dac pull."), target.coordinate.String())
		}
		path, err := service.objectPath(target.object.Digest)
		if err != nil {
			return withAsset(err, target.coordinate.String())
		}
		target.source = path
	}
	return nil
}

// unpackCacheError adds the recovery command to corrupt-object errors.
func unpackCacheError(err error) error {
	if !corrupted(err) {
		return err
	}
	value := fault.As(err)
	return &fault.Error{
		Code:    value.Code,
		Message: "The cache object does not match its digest. Run dac pull.",
		Details: value.Details,
		Cause:   value.Cause,
	}
}

// unpackFilename selects one safe file name from project metadata.
func unpackFilename(name coord.Coordinate, source project.Asset, locked project.LockAsset) (string, error) {
	candidate := source.Filename
	if candidate == "" {
		candidate = locked.Filename
	}
	if candidate == "" {
		candidate = name.Name
	}
	if filename.Clean(candidate) != candidate {
		return "", fmt.Errorf("file name %q is not one safe path element", candidate)
	}
	return candidate, nil
}

// checkUnpackNames rejects selected assets that need the same destination name.
func checkUnpackNames(targets []*unpackTarget) error {
	assets := make(map[string][]string, len(targets))
	for _, target := range targets {
		assets[target.filename] = append(assets[target.filename], target.coordinate.String())
	}
	var files []string
	for name, coordinates := range assets {
		if len(coordinates) > 1 {
			files = append(files, name)
		}
	}
	if len(files) == 0 {
		return nil
	}
	slices.Sort(files)
	var coordinates []string
	var causes []string
	for _, name := range files {
		names := assets[name]
		slices.Sort(names)
		coordinates = append(coordinates, names...)
		causes = append(causes, fmt.Sprintf("%s: %s", name, strings.Join(names, ", ")))
	}
	return &fault.Error{
		Code:    "unpack_name_collision",
		Message: "Two selected assets have the same file name. Select one asset or give one asset another name.",
		Details: map[string]any{"files": files, "assets": coordinates},
		Cause:   errors.New(strings.Join(causes, "; ")),
	}
}

// checkUnpackDestinations rejects unsafe replacements before files are staged.
func checkUnpackDestinations(targets []*unpackTarget, force bool) error {
	var occupied []string
	for _, target := range targets {
		info, err := os.Lstat(target.destination)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fault.Wrap("unpack_write_failed", "DAC could not check a destination file.", err)
		}
		if info.IsDir() {
			return &fault.Error{
				Code:    "unpack_destination_occupied",
				Message: "A destination path is a directory. DAC will not replace a directory.",
				Details: map[string]any{"files": []string{target.destination}},
			}
		}
		if !force {
			occupied = append(occupied, target.destination)
		}
	}
	if len(occupied) == 0 {
		return nil
	}
	slices.Sort(occupied)
	return &fault.Error{
		Code:    "unpack_destination_occupied",
		Message: "The destination contains files that DAC would replace. Use --force to replace them.",
		Details: map[string]any{"files": occupied},
		Cause:   errors.New(strings.Join(occupied, ", ")),
	}
}

// stageUnpack copies and verifies all selected objects before the commit starts.
func stageUnpack(ctx context.Context, targets []*unpackTarget) error {
	for _, target := range targets {
		staged, err := stageUnpackFile(ctx, target.destination, target.source, target.object)
		if err != nil {
			cleanupUnpack(targets)
			if corrupted(err) {
				return withAsset(unpackCacheError(cacheReadError(err)), target.coordinate.String())
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return withAsset(fault.Wrap("cancelled", "The command was cancelled.", err), target.coordinate.String())
			}
			return withAsset(fault.Wrap("unpack_write_failed", "DAC could not stage the asset file.", err), target.coordinate.String())
		}
		target.staged = staged
	}
	return nil
}

// stageUnpackFile creates one verified atomic write beside the destination.
// It stages beside the destination rather than in the cache so that the commit is a
// rename within one file system. Every path out of this operation removes what it
// staged, cancellation included; what none of them can cover is a process that was
// killed outright, which leaves a .dac-unpack-* file behind. Nothing collects
// those, because they are in somebody's directory rather than in DAC's, and a tool
// that deletes files it finds there had better be certain no other unpack is
// running. They are inert, and removing one is a matter for whoever owns the
// directory.
func stageUnpackFile(ctx context.Context, destination, source string, expected Object) (*atomic.File, error) {
	input, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	defer func() { _ = input.Close() }()

	output, err := createUnpackStage(destination)
	if err != nil {
		return nil, err
	}
	failed := true
	defer func() {
		if failed {
			_ = output.Discard()
		}
	}()

	hashValue := sha256.New()
	// Neither a hash nor a MultiWriter implements ReaderFrom, so io.Copy would fall back to its 32 KiB
	// buffer and pay a read and a write syscall for every 32 KiB unpacked. The buffer is sized for the
	// whole-file copies this path actually does.
	written, err := io.CopyBuffer(io.MultiWriter(output, hashValue), &contextReader{ctx: ctx, reader: input}, make([]byte, copyBufferSize))
	if err != nil {
		return nil, err
	}
	actual := digest.Prefix + hex.EncodeToString(hashValue.Sum(nil))
	if written != expected.Size || actual != expected.Digest {
		return nil, &CorruptError{Digest: expected.Digest, ActualDigest: actual, Path: source}
	}
	failed = false
	return output, nil
}

// createUnpackStage keeps staging and commit on one file system and prevents the atomic writer from recreating a checked directory boundary.
func createUnpackStage(destination string) (*atomic.File, error) {
	return atomic.Create(destination, 0o644, atomic.WithTempPrefix(unpackTempPrefix), atomic.WithoutMkdir())
}

// commitUnpack installs all staged files and restores the destination on failure.
func commitUnpack(targets []*unpackTarget, force bool) error {
	for _, target := range targets {
		var err error
		if force {
			target.commit, err = target.staged.CommitReversible()
		} else {
			target.commit, err = target.staged.CommitNoReplace()
			if err != nil {
				err = unpackNoReplaceError(err, target.destination)
			}
		}
		if target.commit != nil {
			target.staged = nil
		}
		if err != nil {
			return failUnpackCommit(targets, err)
		}
	}
	for _, target := range targets {
		if target.commit != nil {
			_ = target.commit.Complete()
			target.commit = nil
		}
	}
	return nil
}

// unpackNoReplaceError names a destination that appeared between validation
// and commit instead of reporting it as an opaque write failure.
func unpackNoReplaceError(err error, destination string) error {
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	return &fault.Error{
		Code:    "unpack_destination_occupied",
		Message: "A destination file appeared while DAC was unpacking. Use --force to replace it.",
		Details: map[string]any{"files": []string{destination}},
		Cause:   err,
	}
}

// failUnpackCommit restores replaced files and removes new files.
// A cause that already carries a code keeps it, because a commit that failed for
// one stated reason is not improved by being called a write failure as well.
func failUnpackCommit(targets []*unpackTarget, cause error) error {
	var rollback []error
	for index := len(targets) - 1; index >= 0; index-- {
		target := targets[index]
		if target.commit != nil {
			if err := target.commit.Rollback(); err != nil {
				rollback = append(rollback, err)
			}
			target.commit = nil
		}
	}
	cleanupUnpack(targets)
	var known *fault.Error
	if errors.As(cause, &known) {
		return &fault.Error{
			Code:    known.Code,
			Message: known.Message,
			Details: known.Details,
			Cause:   errors.Join(append([]error{known.Cause}, rollback...)...),
		}
	}
	return fault.Wrap("unpack_write_failed", "DAC could not commit the asset files.", errors.Join(append([]error{cause}, rollback...)...))
}

// cleanupUnpack removes temporary files that a failed operation did not commit.
func cleanupUnpack(targets []*unpackTarget) {
	for _, target := range targets {
		if target.staged != nil {
			_ = target.staged.Discard()
			target.staged = nil
		}
		if target.commit != nil {
			_ = target.commit.Complete()
			target.commit = nil
		}
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

// Read stops a file copy when the command context ends.
func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
