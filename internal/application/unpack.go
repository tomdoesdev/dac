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

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/filename"
)

// UnpackOptions controls one materialization.
type UnpackOptions struct {
	// Pack is the archive to read.
	Pack string
	// Directory is where the files go. Each asset is written directly into it,
	// under the name its origin gave it, so what comes out is the files
	// themselves rather than a tree to go looking through them in.
	Directory string
	// Assets narrows the unpack to the coordinates these selections name. An
	// empty list is every asset the archive carries.
	//
	// A dacpack holds a whole project, and what wants an asset out of it usually
	// wants one asset: a build step that needs the SDK, an operator checking
	// what a delivery actually contains. Without this the choice was to
	// materialize twenty files and delete nineteen, in a directory the command
	// defaults to the one it was run in.
	Assets []Selection
	// Tree writes each file at the path it has inside the archive --
	// assets/<namespace>/<name>/<version>/<file> -- rather than directly into
	// the destination.
	//
	// It is what a whole-project unpack needs when two assets share a file name,
	// since flat they would land on one path and the unpack is refused. Nothing
	// else can materialize such an archive with its digests checked: tar puts
	// the files in the same places and checks nothing.
	Tree bool
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
//
// ItemCount is what the archive holds and FileCount is what was written, so the
// two differ only when the unpack was narrowed. That is what lets a consumer
// tell "the archive had one asset" from "one asset was asked for", the way
// pull's projectCount does beside its assetCount.
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
// cache import, which reads the same archive and installs the same bytes under
// their digests: one hands a project's assets to something that is not DAC at
// all, and the other moves a cache to another machine that runs it. What comes
// out here is real files with real names, which is the thing the cache cannot
// be -- a cache path is a digest, and nothing that reads an extension can use
// one.
//
// The files land in the destination directory and nowhere below it. An archive
// carries each one under the coordinate it belongs to, because two assets can
// share a name and the archive has to keep them apart, but that layout is a
// property of the container rather than of the assets: what somebody unpacking
// wants is the file, and making them walk four directories to reach it is the
// container's problem leaking out. Tree puts the layout back for the archive
// that needs it, and a name two assets would land on is refused rather than
// resolved -- see checkPackNames.
//
// It reads no project files and needs no cache directory, so it runs anywhere
// the archive does.
//
// Naming assets narrows what is written and nothing else. The whole archive is
// still read and still checked -- an entry the index never listed, a repeat, a
// size that disagrees, a file the index promised and the archive does not carry
// -- because whether this dacpack is sound is not a question a command taking
// one asset out of it gets to skip. What a narrowed unpack does not do is hash
// the contents of a file it was not asked for: those bytes are checked by
// whoever asks for them, and reading them here would cost the narrowing its
// point.
//
// Every name in the archive came from a remote server, so none of them are
// trusted. Each path is recomputed from the coordinate it belongs to before
// anything is read, an index that claims a different one is rejected, and what
// reaches the filesystem is derived rather than taken: the archive's own path
// under --tree, and a file name filename.Clean vouched for otherwise. An
// entry's name is a key for finding that, never a place to write.
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
	// A selection is resolved against the index rather than against what turns
	// up, so an asset this archive does not carry is refused before a byte of it
	// is written -- for the reason a narrowed pull refuses one: a typo should
	// unpack nothing rather than most of what was asked for.
	wanted, err := wantedPackFiles(targets, options.Assets)
	if err != nil {
		return UnpackResult{}, err
	}
	// Two assets landing on one name is a question about the archive and the
	// selection rather than about the disk, so it is answered before the disk is
	// asked anything.
	if !options.Tree {
		if err := checkPackNames(wanted); err != nil {
			return UnpackResult{}, err
		}
	}
	// The index is the first entry, so every destination is known before a
	// single file has been read. Refusing here rather than on the way past is
	// what keeps a collision from leaving half a tree behind.
	if err := checkPackDestinations(directory, wanted, options.Tree, options.Force); err != nil {
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
		seen[target.path] = struct{}{}
		// An entry this unpack was not narrowed to has now been accounted for,
		// which is all a narrowed unpack owes it. Next skips the rest of it
		// without reading the bytes.
		if _, chosen := wanted[target.path]; !chosen {
			continue
		}
		destination, err := unpackDestination(directory, target, options.Tree)
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
			Coordinate: target.coordinate.String(),
			Path:       destination,
			Filename:   target.item.Filename,
			Digest:     target.object.Digest,
			Size:       target.object.Size,
		})
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

// wantedPackFiles returns the files one unpack should write, keyed by the
// derived path its targets already are.
//
// No selection at all is every file the index lists, which is what an unpack
// was before it could be narrowed. Anything else is resolved against the
// coordinates the index carries rather than against the file names, because a
// coordinate is what the caller names an asset by and what the index is unique
// on -- two items claiming one coordinate had the archive rejected already, so
// one coordinate here means one file.
func wantedPackFiles(targets map[string]packTarget, selections []Selection) (map[string]packTarget, error) {
	if len(selections) == 0 {
		return targets, nil
	}
	byCoordinate := make(map[coord.Coordinate]packTarget, len(targets))
	for _, target := range targets {
		byCoordinate[target.coordinate] = target
	}
	names, err := chosen(subjectPack, byCoordinate, selections)
	if err != nil {
		return nil, err
	}
	wanted := make(map[string]packTarget, len(names))
	for _, name := range names {
		target := byCoordinate[name]
		wanted[target.path] = target
	}
	return wanted, nil
}

// unpackDestination returns where one file goes on disk under either layout.
//
// The two differ in what they are allowed to be -- an archive path or a file
// name -- so each is checked against its own claim rather than against a rule
// wide enough to admit both.
func unpackDestination(directory string, target packTarget, tree bool) (string, error) {
	if tree {
		return packDestination(directory, target.path)
	}
	return flatDestination(directory, target.item.Filename)
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
	return containedPath(directory, file)
}

// flatDestination puts one file directly in the destination directory, under
// the name its origin gave it.
//
// The name is checked against filename.Clean rather than trusted, for the
// reason the archive path is checked against the asset root: validatePackIndex
// proved this already -- a name that is not one safe path element could not
// have produced the derived path the index had to match -- and this is where
// that proof is worth having, one line from the write. A name is one path
// element or it is not a name, and repairing it would invent a claim the origin
// never made.
func flatDestination(directory, name string) (string, error) {
	if name == "" || filename.Clean(name) != name {
		return "", fmt.Errorf("file name %q is not one safe path element", name)
	}
	return containedPath(directory, name)
}

// containedPath joins one checked relative path onto the destination and
// refuses a result that is not inside it. It is the last thing standing between
// a name that came off a network and a file DAC creates, so both layouts end
// here whatever else they checked on the way.
func containedPath(directory, relative string) (string, error) {
	destination := filepath.Join(directory, filepath.FromSlash(relative))
	inside, err := filepath.Rel(directory, destination)
	if err != nil {
		return "", fmt.Errorf("file %q does not resolve inside the destination: %w", relative, err)
	}
	if inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("file %q resolves outside the destination", relative)
	}
	return destination, nil
}

// checkPackNames refuses an unpack whose files would land on one name.
//
// Flat, a file is its origin's name and nothing else, and two assets can carry
// the same one -- two versions of one asset usually do, since a version is
// rarely in the file name, and two namespaces holding a geo.bin each is
// ordinary. The archive keeps them apart by coordinate; a destination has
// nothing to keep them apart with.
//
// So it refuses rather than picking. Writing both would leave one asset's bytes
// under a name the result says belongs to the other, which is the failure this
// command works hardest to avoid: a file that looks like what was asked for and
// is not. Renaming one would invent a name no origin gave. --force does not
// reach here either, because force is permission to replace what is already on
// disk, not permission to lose one of the two files this command was told to
// write.
//
// It names every clash rather than the first, for the reason the destination
// check does: the answer is usually to name the asset that was wanted, and
// finding out one file at a time is worse than being told.
func checkPackNames(wanted map[string]packTarget) error {
	assets := make(map[string][]string, len(wanted))
	for _, target := range wanted {
		name := target.item.Filename
		assets[name] = append(assets[name], target.coordinate.String())
	}
	files := make([]string, 0, len(assets))
	for name, coordinates := range assets {
		if len(coordinates) > 1 {
			files = append(files, name)
		}
	}
	if len(files) == 0 {
		return nil
	}
	// Map order is not an order, and an operator reading a refusal wants the
	// same one every time.
	slices.Sort(files)
	clashing := make([]string, 0, len(files))
	reasons := make([]string, 0, len(files))
	for _, name := range files {
		coordinates := assets[name]
		slices.Sort(coordinates)
		clashing = append(clashing, coordinates...)
		reasons = append(reasons, fmt.Sprintf("%s: %s", name, strings.Join(coordinates, ", ")))
	}
	return &fault.Error{
		Code:    "unpack_name_collision",
		Message: "Two assets in the dacpack have the same file name, and both cannot be written to it. Name the asset you want, or use --tree.",
		Details: map[string]any{"files": files, "assets": clashing},
		Cause:   fmt.Errorf("%s", strings.Join(reasons, "; ")),
	}
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
//
// It is given the files this unpack will write rather than every file the
// archive holds, so an unpack narrowed to one asset is refused by what is in
// that asset's way and not by what is in another's.
func checkPackDestinations(directory string, wanted map[string]packTarget, tree, force bool) error {
	if force {
		return nil
	}
	occupied := make([]string, 0, len(wanted))
	for _, target := range wanted {
		destination, err := unpackDestination(directory, target, tree)
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
