package dac

// This file holds the boundary DAC records what its cache objects are across.

import (
	"time"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/project"
)

// CatalogEntry is one thing a run learned about the bytes under one digest.
type CatalogEntry struct {
	Digest     string
	Size       int64
	Coordinate coord.Coordinate
	URL        string
	Filename   string
}

// CatalogRecord is what DAC remembers one object to be.
// KnownAs names every coordinate the object has been recorded under, which is a weaker claim
// than a project's own coordinates: those resolve to the object now, these once did.
type CatalogRecord struct {
	Filename  string
	SourceURL string
	KnownAs   []string
	FirstSeen time.Time
}

// Catalog keeps a description of the objects DAC has downloaded, so that a cache can still be
// explained after the project files that named its contents change or disappear.
//
// A nil catalog records nothing and remembers nothing, which is what a run whose catalog file
// could not be reached does. None of this is an answer a command was asked for, so none of it
// may fail one.
type Catalog interface {
	// Note adds what a run learned. It cannot fail, because it only adds to memory: the file is
	// written once, at the end of the run.
	Note(entries []CatalogEntry)
	// Forget drops the whole record of each object, for objects that have left the cache.
	Forget(digests []string)
	// Describe reports what DAC remembers one object to be, for a listing to name it.
	Describe(digest string) (CatalogRecord, bool)
}

// note records the assets a command has just read a lock file for.
//
// A locked asset carries the URL, the digest, the size, and the settled file name together, so
// the manifest beside it would add nothing here. An entry with no digest is one no lock file
// describes yet, and there are no bytes to record.
func (service *Service) note(assets map[coord.Coordinate]project.LockAsset) {
	if service.Catalog == nil || len(assets) == 0 {
		return
	}
	entries := make([]CatalogEntry, 0, len(assets))
	for name, locked := range assets {
		if locked.Digest == "" {
			continue
		}
		entries = append(entries, CatalogEntry{
			Digest:     locked.Digest,
			Size:       locked.Size,
			Coordinate: name,
			URL:        locked.URL,
			Filename:   locked.Filename,
		})
	}
	service.Catalog.Note(entries)
}

// noteAsset records one asset as its bytes reach the cache.
//
// This is separate from note because the two answer different risks. Note records what a whole
// project says, once the operation reading it has settled. This records bytes the moment they
// land, before anything else can fail: an operation that is cancelled, or that gives up because
// a different asset failed, still leaves behind everything it had already downloaded, and those
// are precisely the objects nothing would otherwise be able to name.
func (service *Service) noteAsset(name coord.Coordinate, locked project.LockAsset) {
	service.note(map[coord.Coordinate]project.LockAsset{name: locked})
}

// forget drops the records of objects a command has removed from the cache.
func (service *Service) forget(digests []string) {
	if service.Catalog == nil || len(digests) == 0 {
		return
	}
	service.Catalog.Forget(digests)
}

// describe reports what DAC remembers one object to be, and nothing without a catalog to ask.
func (service *Service) describe(digest string) (CatalogRecord, bool) {
	if service.Catalog == nil {
		return CatalogRecord{}, false
	}
	return service.Catalog.Describe(digest)
}
