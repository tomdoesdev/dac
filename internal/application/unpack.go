package application

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tom/dac/internal/digest"
	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/jsonfile"
)

// UnpackOptions controls one materialization.
type UnpackOptions struct {
	// Pack is the archive to read.
	Pack string
	// Directory is where the files go. Every file in the archive is written at
	// the same path beneath it, so the mapping from archive to disk is the
	// identity and the paths the index was already checked against are the paths
	// that reach the filesystem.
	Directory string
	// Force replaces files that are already there. Without it an unpack that
	// would overwrite anything writes nothing at all: the default directory is
	// wherever the command was run, and a mistake there costs work that was not
	// DAC's to lose.
	Force bool
}

// UnpackedFile reports one file an unpack wrote.
type UnpackedFile struct {
	Coordinate string `json:"coordinate"`
	Path       string `json:"path"`
	Filename   string `json:"filename"`
	Digest     string `json:"digest"`
	Size       int64  `json:"size"`
}

// UnpackResult reports the files materialized from one dacpack.
type UnpackResult struct {
	Pack      string         `json:"pack"`
	Directory string         `json:"directory"`
	ItemCount int            `json:"itemCount"`
	FileCount int            `json:"fileCount"`
	ByteCount int64          `json:"byteCount"`
	Files     []UnpackedFile `json:"files"`
}

// Unpack writes the assets a dacpack carries into a directory.
//
// It never touches the cache. That is the whole difference between this and
// import: a cache bundle is how DAC moves objects to another DAC, and a dacpack
// is how a project hands its assets to something that is not DAC at all. What
// comes out is a tree of real files with real names, which is the thing the
// cache cannot be -- a cache path is a digest, and nothing that reads an
// extension can use one.
//
// It reads no project files and needs no cache directory, so it runs anywhere
// the archive does.
//
// Every path in the archive came from a name a remote server chose, so none of
// them are trusted. Each is recomputed from the coordinate it belongs to before
// anything is read, an index that claims a different one is rejected, and the
// path that reaches the filesystem is the one DAC derived rather than the one
// the archive carried. An entry's own name is a key for finding that, never a
// place to write.
func (service *Service) Unpack(ctx context.Context, options UnpackOptions) (UnpackResult, error) {
	absolutePack, err := filepath.Abs(options.Pack)
	if err != nil {
		return UnpackResult{}, fault.Wrap("unpack_read_failed", "DAC could not resolve the dacpack path.", err)
	}
	directory, err := filepath.Abs(options.Directory)
	if err != nil {
		return UnpackResult{}, fault.Wrap("unpack_write_failed", "DAC could not resolve the destination directory.", err)
	}
	file, err := os.Open(absolutePack)
	if err != nil {
		return UnpackResult{}, fault.Wrap("unpack_read_failed", "DAC could not read the dacpack.", err)
	}
	defer func() { _ = file.Close() }()

	reader := tar.NewReader(file)
	index, targets, err := readPackIndex(reader)
	if err != nil {
		return UnpackResult{}, invalidPack(err)
	}
	// The index is the first entry, so every destination is known before a
	// single file has been read. Refusing here rather than on the way past is
	// what keeps a collision from leaving half a tree behind.
	if err := checkPackDestinations(directory, targets, options.Force); err != nil {
		return UnpackResult{}, err
	}

	result := UnpackResult{
		Pack:      absolutePack,
		Directory: directory,
		ItemCount: len(index.Items),
		Files:     []UnpackedFile{},
	}
	// An archive is only known to be sound once it has been read to the end, and
	// by then some of it has already been written. Undoing that is what keeps a
	// rejected dacpack from leaving a tree that looks like a whole one.
	written := &materializer{}
	failed := func(err error) (UnpackResult, error) {
		written.rollback()
		return UnpackResult{}, err
	}
	seen := make(map[string]struct{}, len(targets))
	for {
		if err := ctx.Err(); err != nil {
			return failed(networkError(err))
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return failed(invalidPack(err))
		}
		// The entry's name is a lookup key and never a path. What it finds
		// carries the path DAC derived, which is what gets written to.
		target, exists := targets[header.Name]
		if !exists {
			return failed(invalidPack(fmt.Errorf("dacpack has unexpected file %q", header.Name)))
		}
		if _, duplicate := seen[target.path]; duplicate {
			return failed(invalidPack(fmt.Errorf("dacpack has duplicate file %q", target.path)))
		}
		if !regularTarFile(header) {
			return failed(invalidPack(fmt.Errorf("dacpack file %q is not a regular file", target.path)))
		}
		if header.Size != target.object.Size {
			return failed(invalidPack(fmt.Errorf("dacpack file %q has size %d, not %d", target.path, header.Size, target.object.Size)))
		}
		destination, err := packDestination(directory, target.path)
		if err != nil {
			return failed(invalidPack(err))
		}
		if err := written.write(ctx, destination, reader, target.object); err != nil {
			var content *ContentError
			switch {
			case errors.As(err, &content), errors.Is(err, io.ErrUnexpectedEOF):
				return failed(invalidPack(err))
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				return failed(networkError(err))
			default:
				return failed(fault.Wrap("unpack_write_failed", "DAC could not write the asset file.", err))
			}
		}
		result.Files = append(result.Files, UnpackedFile{
			Coordinate: target.item.Coordinate,
			Path:       destination,
			Filename:   target.item.Filename,
			Digest:     target.object.Digest,
			Size:       target.object.Size,
		})
		seen[target.path] = struct{}{}
		result.FileCount++
		result.ByteCount += target.object.Size
	}
	if len(seen) != len(targets) {
		for path := range targets {
			if _, exists := seen[path]; !exists {
				return failed(invalidPack(fmt.Errorf("dacpack file %q is missing", path)))
			}
		}
	}
	return result, nil
}

// packDestination joins one derived archive path onto the destination
// directory, and refuses a result that is not inside it.
//
// Nothing should ever be able to reach this check: the path was built from a
// coordinate whose three parts coord validated and a file name filename.Clean
// vouched for, so it holds no traversal to follow. It is here so that the
// guarantee is provable at the line that touches the filesystem rather than
// four files away, and so that a future change to the derivation cannot quietly
// turn into a write outside the directory the caller named.
func packDestination(directory, file string) (string, error) {
	// An absolute path is refused outright rather than left to the containment
	// check below, which would pass it: Join cleans the leading separator, so
	// "/etc/passwd" lands harmlessly at "<directory>/etc/passwd". Harmless is
	// not the same as derived, and every derived path starts at the asset root.
	if path.IsAbs(file) || !strings.HasPrefix(file, packAssetRoot+"/") {
		return "", fmt.Errorf("file %q is not a path DAC derived", file)
	}
	destination := filepath.Join(directory, filepath.FromSlash(file))
	inside, err := filepath.Rel(directory, destination)
	if err != nil {
		return "", fmt.Errorf("file %q does not resolve inside the destination: %w", file, err)
	}
	if inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("file %q resolves outside the destination", file)
	}
	return destination, nil
}

// materializer writes the files of one unpack and can undo them.
//
// A dacpack is not known to be sound until it has been read to the end: an
// entry the index never listed, or bytes that fail their digest, can arrive
// after perfectly good files have already been written. Leaving those behind
// would be the failure this command works hardest to avoid -- a tree that looks
// complete and is not, handed to a script that has no way to tell.
//
// It undoes only what it created. A file that --force replaced is not restored,
// because the thing it replaced is already gone and removing the replacement
// would leave neither; the directories it made are removed only while empty, so
// nothing that was already there goes with them.
type materializer struct {
	created     []string
	directories []string
}

func (writer *materializer) write(ctx context.Context, target string, source io.Reader, expected Object) error {
	existed := true
	if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
		existed = false
	} else if err != nil {
		return err
	}
	if err := writer.ensureDirectory(filepath.Dir(target)); err != nil {
		return err
	}
	if err := writeMaterializedFile(ctx, target, source, expected); err != nil {
		return err
	}
	if !existed {
		writer.created = append(writer.created, target)
	}
	return nil
}

// ensureDirectory creates one directory and every missing parent, recording
// each one it had to make so that rollback takes back exactly those.
func (writer *materializer) ensureDirectory(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if parent := filepath.Dir(path); parent != path {
		if err := writer.ensureDirectory(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		// Something else created it between the stat and here, which is the
		// answer this wanted anyway.
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return err
	}
	writer.directories = append(writer.directories, path)
	return nil
}

// rollback removes what this unpack added, deepest first. Every failure here is
// ignored: the command is already failing, and a cleanup that cannot finish is
// not a better error than the one that caused it.
func (writer *materializer) rollback() {
	for index := len(writer.created) - 1; index >= 0; index-- {
		_ = os.Remove(writer.created[index])
	}
	for index := len(writer.directories) - 1; index >= 0; index-- {
		_ = os.Remove(writer.directories[index])
	}
}

// checkPackDestinations refuses an unpack that would replace anything.
//
// It names every collision rather than the first, because the answer to one is
// usually to unpack somewhere else, and finding that out a file at a time is
// worse than being told.
func checkPackDestinations(directory string, targets map[string]packTarget, force bool) error {
	if force {
		return nil
	}
	occupied := make([]string, 0, len(targets))
	for _, target := range targets {
		destination, err := packDestination(directory, target.path)
		if err != nil {
			return invalidPack(err)
		}
		// Lstat rather than Stat: a symlink where a file is going is something
		// already there, and following it is how an unpack writes somewhere it
		// was never pointed at.
		if _, err := os.Lstat(destination); err == nil {
			occupied = append(occupied, destination)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fault.Wrap("unpack_write_failed", "DAC could not check the destination directory.", err)
		}
	}
	// Map order is not an order, and an operator reading a list of files wants
	// the same one every time.
	slices.Sort(occupied)
	if len(occupied) == 0 {
		return nil
	}
	return &fault.Error{
		Code:    "unpack_destination_occupied",
		Message: "The destination already holds files this dacpack would replace. Unpack somewhere else, or use --force.",
		Details: map[string]any{"files": occupied},
		Cause:   fmt.Errorf("%s", strings.Join(occupied, ", ")),
	}
}

// writeMaterializedFile writes one asset through a temporary file and one
// rename, checking the bytes on the way.
//
// The digest is checked while writing rather than afterwards, because the bytes
// are already going somewhere and reading them back to check would double the
// work. The rename is what makes a failed check leave nothing: a file that did
// not match never reaches the name it claimed.
func writeMaterializedFile(ctx context.Context, target string, source io.Reader, expected Object) error {
	directory := filepath.Dir(target)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".dac-unpack-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()

	hashValue := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hashValue), &contextReader{
		ctx:    ctx,
		reader: io.LimitReader(source, expected.Size),
	})
	if err != nil {
		return err
	}
	actual := digest.Prefix + hex.EncodeToString(hashValue.Sum(nil))
	if written != expected.Size || actual != expected.Digest {
		return &ContentError{
			ExpectedDigest: expected.Digest,
			ActualDigest:   actual,
			ExpectedSize:   expected.Size,
			ActualSize:     written,
		}
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Chmod(0o644); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	// Remove first so that a replacement lands on the name rather than through
	// whatever is currently answering to it. Only --force reaches this with
	// anything in the way; without it the destination check already refused.
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(temporaryPath, target)
}

// readPackIndex reads and checks the first tar entry.
func readPackIndex(reader *tar.Reader) (packIndex, map[string]packTarget, error) {
	header, err := reader.Next()
	if err != nil {
		return packIndex{}, nil, err
	}
	if header.Name != packIndexPath || !regularTarFile(header) {
		return packIndex{}, nil, errors.New("the first dacpack entry must be a regular index.json file")
	}
	if header.Size > maximumIndexSize {
		return packIndex{}, nil, fmt.Errorf("dacpack index is larger than %d bytes", maximumIndexSize)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return packIndex{}, nil, err
	}
	var index packIndex
	if err := jsonfile.DecodeStrict(data, &index); err != nil {
		return packIndex{}, nil, err
	}
	targets, err := validatePackIndex(index)
	return index, targets, err
}

// invalidPack gives all dacpack format failures one stable command error.
func invalidPack(err error) error {
	return fault.Wrap("dacpack_invalid", "The dacpack is invalid.", err)
}
