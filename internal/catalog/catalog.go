// Package catalog records what the objects in the cache are.
//
// The cache is content-addressed, so an object on disk is named by its digest and by nothing
// else. What that object is — a file name, a coordinate, the URL it came from — lives only in
// the lock file of a project that happens to reference it. Change or delete that project and
// the bytes stay while every way of identifying them goes, which leaves a cache full of objects
// nobody can account for.
//
// The catalog is the record that survives the project. It is written at the root of the cache it
// describes, beside the objects rather than among them: collection walks the object directories
// and never the root, so emptying a cache cannot take the record of it away.
package catalog

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
)

// SchemaVersion is the catalog file version DAC reads and writes.
const SchemaVersion = 1

// FileName is the catalog, which sits at the root of the cache it describes.
const FileName = "catalog.json"

// fileMode is the permission the catalog file carries.
const fileMode = 0o644

// Source is one identity an object has been seen under.
// Two coordinates can resolve to the same bytes, and one coordinate can carry a different URL
// or file name from another, so an object holds a source per coordinate rather than one name.
type Source struct {
	URL      string `json:"url"`
	Filename string `json:"filename,omitempty"`
	// FirstSeen is when this coordinate first resolved to these bytes, which is what separates
	// a name an asset has always had from one it moved to.
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

// Object is what DAC knows about the bytes one digest names.
type Object struct {
	Size      int64     `json:"size"`
	FirstSeen time.Time `json:"firstSeen"`
	// LastSeen is the newest of this object's sources by construction. It is stored rather than
	// derived so that sorting and collection read one field instead of walking every source.
	LastSeen time.Time                   `json:"lastSeen"`
	Sources  map[coord.Coordinate]Source `json:"sources"`
}

// Catalog is the catalog file as one value.
// Objects is keyed by digest because the question the catalog answers is what a given object
// is, and because that is the key the cache, collection, and every listing already work in.
type Catalog struct {
	SchemaVersion int               `json:"schemaVersion"`
	Objects       map[string]Object `json:"objects"`
}

// Entry is one observation of an object, made while a command runs.
type Entry struct {
	Digest     string
	Size       int64
	Coordinate coord.Coordinate
	URL        string
	Filename   string
}

// Empty returns a catalog that records nothing.
func Empty() Catalog {
	return Catalog{SchemaVersion: SchemaVersion, Objects: map[string]Object{}}
}

// Validate checks the schema version and every digest the catalog names.
func (catalog Catalog) Validate() error {
	if catalog.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported catalog schema version %d, DAC understands %d",
			catalog.SchemaVersion, SchemaVersion)
	}
	if catalog.Objects == nil {
		return errors.New("catalog objects must be an object")
	}
	for value, object := range catalog.Objects {
		if _, err := digest.Hex(value); err != nil {
			return fmt.Errorf("catalog names %q, which is not a digest: %w", value, err)
		}
		if object.Size < 0 {
			return fmt.Errorf("catalog gives %s a negative size", value)
		}
	}
	return nil
}

// Note records what a run observed, and returns the catalog that results.
// An object already recorded keeps the time it was first seen: the record says how long DAC has
// held these bytes, which a re-download does not reset.
func (catalog Catalog) Note(entries []Entry, now time.Time) Catalog {
	updated := catalog.clone()
	for _, entry := range entries {
		if entry.Digest == "" {
			continue
		}
		object, existed := updated.Objects[entry.Digest]
		if !existed {
			object.FirstSeen = now
		}
		object.Size = entry.Size
		object.LastSeen = now
		// The sources map is replaced rather than written through, because clone copies each
		// object by value and a map field copied by value is the same map.
		sources := maps.Clone(object.Sources)
		if sources == nil {
			sources = map[coord.Coordinate]Source{}
		}
		source, seen := sources[entry.Coordinate]
		if !seen {
			source.FirstSeen = now
		}
		source.URL = entry.URL
		source.Filename = entry.Filename
		source.LastSeen = now
		sources[entry.Coordinate] = source
		object.Sources = sources
		updated.Objects[entry.Digest] = object
	}
	return updated
}

// Forget drops the whole record of each object named, and reports the ones it held.
// A record describes what the cache holds, so an object that has left the cache leaves its
// record with it rather than becoming a claim about bytes that are no longer there.
func (catalog Catalog) Forget(digests []string) (Catalog, []string) {
	updated := catalog.clone()
	forgotten := []string{}
	for _, value := range digests {
		if _, held := updated.Objects[value]; !held {
			continue
		}
		delete(updated.Objects, value)
		forgotten = append(forgotten, value)
	}
	slices.Sort(forgotten)
	return updated, forgotten
}

// Coordinates returns the coordinates this object has been seen under, in name order.
func (object Object) Coordinates() []string {
	names := make([]string, 0, len(object.Sources))
	for name := range object.Sources {
		names = append(names, name.String())
	}
	slices.Sort(names)
	return names
}

// Newest returns the source seen most recently, which is the name to call an object by.
// Ties break on the coordinate so that one catalog always answers this the same way.
func (object Object) Newest() (Source, bool) {
	var newest Source
	var chosen coord.Coordinate
	found := false
	for name, source := range object.Sources {
		better := !found ||
			source.LastSeen.After(newest.LastSeen) ||
			(source.LastSeen.Equal(newest.LastSeen) && coord.Compare(name, chosen) < 0)
		if !better {
			continue
		}
		newest, chosen, found = source, name, true
	}
	return newest, found
}

// clone copies the catalog so every operation returns a new value.
func (catalog Catalog) clone() Catalog {
	objects := maps.Clone(catalog.Objects)
	if objects == nil {
		objects = map[string]Object{}
	}
	return Catalog{SchemaVersion: SchemaVersion, Objects: objects}
}
