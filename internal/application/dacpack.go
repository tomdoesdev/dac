package application

// This file defines the archive that pack writes, and that unpack and cache
// import read.
//
// A dacpack is the project, materialized. Its files are named the way the
// origins name them, so the archive is useful to something that is not DAC: it
// can be extracted and read by a tool that switches on an extension, which a
// directory of sha256 hashes cannot be.
//
// DAC used to carry a second archive for its own use. A cache bundle held the
// same bytes under their digests, and that layout made import's validation
// free: the only path an item could claim was the one its digest spelled, so
// nothing written into the index could point anywhere else. It cost two
// formats, two commands to write them, and an operator choosing between them
// before knowing which of the two things they wanted the file for -- and the
// choice was rarely the one they wanted, because a bundle only ever reached
// another DAC. One archive that both halves read is worth more than a check
// that came for nothing, so a dacpack is what moves a cache now.
//
// The price is that a name that came from a remote server is part of a path.
// Everything below exists to give that path back the property the digest layout
// had by construction.

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"path"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/filename"
	"github.com/tomdoesdev/dac/internal/jsonfile"
	"github.com/tomdoesdev/dac/internal/project"
)

const (
	packSchemaVersion = 1
	packIndexPath     = "index.json"
	// packAssetRoot keeps every materialized file under one directory, so the
	// index is the only thing at the root of the archive and extracting one
	// cannot scatter files across the directory it was extracted into.
	packAssetRoot = "assets"
	// maximumIndexSize bounds the one entry DAC reads into memory. Everything
	// else in an archive is streamed, so this is the only place a hand-written
	// file could ask for as much memory as it liked.
	maximumIndexSize = 16 << 20
)

// DefaultPackFile is the archive pack writes and unpack reads when a command
// names none.
//
// A dacpack is a build output: the common case is one per project, made and
// consumed by scripts that should not each invent a spelling for it. Import
// takes no default, because a pack it reads arrived from somewhere else and was
// put down wherever whoever delivered it chose.
const DefaultPackFile = "dac.dacpack"

// PackItem describes one materialized file in a dacpack.
//
// It carries the digest even though unpack writes files rather than cache
// objects, because the digest is what makes the archive checkable: a file name
// says nothing about the bytes under it, and this is the only claim about them
// there is. It is also what ties the file back to the coordinate's entry in a
// lock file, for a consumer that has one.
//
// File is the path inside the archive and Filename is the name at the end of
// it. Both are recorded rather than one derived from the other, because they
// answer different questions -- where this is, and what to call it -- and a
// consumer that only wants to know what the origin called an asset should not
// have to parse a path to find out.
type PackItem struct {
	Coordinate string `json:"coordinate"`
	SourceURL  string `json:"sourceUrl"`
	File       string `json:"file"`
	Filename   string `json:"filename"`
	Digest     string `json:"digest"`
	Size       int64  `json:"size"`
}

// packIndex is the root document in a dacpack.
type packIndex struct {
	SchemaVersion int        `json:"schemaVersion"`
	Items         []PackItem `json:"items"`
}

// packFileName returns the name one asset is materialized under.
//
// The lock records what the origin called the asset, which is the name worth
// having. It is advisory and may be missing -- from a URL that ends in a slash,
// from a header DAC refused, or from a lock written before names were recorded
// -- so it falls back to the name half of the coordinate, which every
// coordinate has and which coord has already checked is one safe path element.
func packFileName(name coord.Coordinate, locked project.LockAsset) string {
	if clean := filename.Clean(locked.Filename); clean != "" {
		return clean
	}
	return name.Name
}

// packFilePath returns the only path a dacpack may carry one asset at.
//
// The path is derived, never taken. A reader recomputes it from the coordinate
// and the name in the index and refuses an item that claims anything else, so a
// hand-written index naming "../../../etc/cron.d/root" is rejected before
// anything reads the file, not after. That is the property a digest layout had
// by construction, since the only path an item could claim was the one its
// digest spelled.
//
// The coordinate supplies the directories because it is unique by definition
// and coord has already checked that all three of its parts are single path
// elements. Two versions of one asset almost always share a file name, and two
// assets easily can, so a layout keyed on the name alone would have items
// collide -- silently, since the later one would simply overwrite the earlier.
func packFilePath(name coord.Coordinate, file string) (string, error) {
	if err := name.Validate(); err != nil {
		return "", err
	}
	if file == "" || filename.Clean(file) != file {
		return "", fmt.Errorf("file name %q is not one safe path element", file)
	}
	return path.Join(packAssetRoot, name.Namespace, name.Name, name.Version, file), nil
}

// packTarget is one file a validated index describes.
//
// Path is the path DAC derived from the coordinate, not the one the index
// claimed. The two are equal or the index was rejected, so this changes no
// behaviour -- what it changes is provenance. The value that reaches the
// filesystem is one DAC computed from data it had already checked, rather than
// a string out of the archive that a reader has to trace back through the
// equality test to know is safe.
//
// Coordinate is the parsed form of the same claim, kept for the reason the path
// is: it is what an unpack narrowed to one asset matches against, and matching
// on the string would ask each reader to trust that the string parsed.
type packTarget struct {
	item       PackItem
	coordinate coord.Coordinate
	path       string
	object     Object
}

// validatePackIndex checks item metadata and returns what belongs at each path.
//
// It is keyed by path rather than by object. A dacpack materializes each asset
// separately, so two coordinates that resolved to the same bytes appear as two
// files with two paths and one digest, and both readers need to know what to
// expect at each -- unpack to write it, import to check it before the cache
// takes it.
func validatePackIndex(index packIndex) (map[string]packTarget, error) {
	if index.SchemaVersion != packSchemaVersion {
		return nil, fmt.Errorf("unsupported dacpack schema version %d", index.SchemaVersion)
	}
	if index.Items == nil {
		return nil, fmt.Errorf("dacpack items must be an array")
	}
	coordinates := make(map[coord.Coordinate]struct{}, len(index.Items))
	targets := make(map[string]packTarget, len(index.Items))
	for position, item := range index.Items {
		coordinate, err := coord.Parse(item.Coordinate)
		if err != nil {
			return nil, fmt.Errorf("dacpack item %d: %w", position, err)
		}
		if item.SourceURL == "" {
			return nil, fmt.Errorf("dacpack item %d has no source URL", position)
		}
		if item.Size < 0 {
			return nil, fmt.Errorf("dacpack item %d has a negative size", position)
		}
		if _, err := digest.Hex(item.Digest); err != nil {
			return nil, fmt.Errorf("dacpack item %d has an invalid digest: %w", position, err)
		}
		derived, err := packFilePath(coordinate, item.Filename)
		if err != nil {
			return nil, fmt.Errorf("dacpack item %d: %w", position, err)
		}
		if item.File != derived {
			return nil, fmt.Errorf("dacpack item %d has file %q, not %q", position, item.File, derived)
		}
		if _, exists := coordinates[coordinate]; exists {
			return nil, fmt.Errorf("dacpack has duplicate item %q", coordinate)
		}
		coordinates[coordinate] = struct{}{}
		// Keyed on the derived path rather than the claimed one, so that even the
		// lookup an archive entry performs is against a path DAC built.
		//
		// One path per coordinate and one coordinate per item, so a repeated path
		// here means two items disagree about which asset a file belongs to.
		if _, exists := targets[derived]; exists {
			return nil, fmt.Errorf("dacpack has duplicate file %q", derived)
		}
		targets[derived] = packTarget{
			item:       item,
			coordinate: coordinate,
			path:       derived,
			object:     Object{Digest: item.Digest, Size: item.Size},
		}
	}
	return targets, nil
}

// readPackIndex reads and checks the first tar entry.
//
// Both readers start here, because both have to know what the archive claims
// before the first file arrives: unpack to know where a file may be written and
// import to know what bytes it must turn out to be. Reading it any later would
// mean buffering the archive to find out.
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

// regularTarFile accepts the current and historical regular-file type flags.
func regularTarFile(header *tar.Header) bool {
	return header.Typeflag == tar.TypeReg || header.Typeflag == 0
}

// invalidPack gives all dacpack format failures one stable command error.
func invalidPack(err error) error {
	return fault.Wrap("dacpack_invalid", "The dacpack is invalid.", err)
}
