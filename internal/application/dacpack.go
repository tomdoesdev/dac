package application

// This file defines the archive that pack writes and unpack reads.
//
// A cache bundle and a dacpack carry the same bytes and answer different
// questions. A bundle is the cache, moved: every file in it is named by its
// digest, which is what makes import's validation trivial -- the only path an
// item may claim is the one its digest spells, so nothing an attacker writes in
// the index can point anywhere else.
//
// A dacpack is the project, materialized. Its files are named the way the
// origins name them, so the archive is useful to something that is not DAC: it
// can be extracted and read by a tool that switches on an extension, which a
// directory of sha256 hashes cannot be. That is the whole point, and it is also
// what costs the free validation, because a name that came from a remote server
// is now part of a path. Everything below exists to give that path back the
// property the digest layout got for nothing.

import (
	"fmt"
	"path"

	"github.com/tom/dac/internal/coord"
	"github.com/tom/dac/internal/digest"
	"github.com/tom/dac/internal/filename"
	"github.com/tom/dac/internal/project"
)

const (
	packSchemaVersion = 1
	packIndexPath     = "index.json"
	// packAssetRoot keeps every materialized file under one directory, so the
	// index is the only thing at the root of the archive and extracting one
	// cannot scatter files across the directory it was extracted into.
	packAssetRoot = "assets"
)

// DefaultPackFile is the archive pack writes and unpack reads when a command
// names none.
//
// Export takes a required --file because a cache bundle is a thing you are
// moving somewhere and the destination is the point. A dacpack is a build
// output: the common case is one per project, made and consumed by scripts that
// should not each invent a spelling for it.
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
// This is the dacpack's answer to bundleBlobPath, and it is doing the same job:
// the path is derived, never taken. Unpack recomputes it from the coordinate
// and the name in the index and refuses an item that claims anything else, so a
// hand-written index naming "../../../etc/cron.d/root" is rejected before
// anything reads the file, not after.
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
type packTarget struct {
	item   PackItem
	path   string
	object Object
}

// validatePackIndex checks item metadata and returns what belongs at each path.
//
// It returns a map keyed by path rather than the set of distinct objects a
// bundle returns. A dacpack materializes each asset separately, so two
// coordinates that resolved to the same bytes appear as two files with two
// paths and one digest, and the reader needs to know what to expect at each.
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
			item:   item,
			path:   derived,
			object: Object{Digest: item.Digest, Size: item.Size},
		}
	}
	return targets, nil
}
