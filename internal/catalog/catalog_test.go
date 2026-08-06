package catalog

import (
	"slices"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
)

var (
	first  = digest.Bytes([]byte("first object"))
	second = digest.Bytes([]byte("second object"))
	early  = time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	later  = time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)
)

func entry(value string, name string, url, file string) Entry {
	return Entry{Digest: value, Size: 12, Coordinate: coord.MustParse(name), URL: url, Filename: file}
}

func TestNoteRecordsWhatAnObjectIsCalled(t *testing.T) {
	recorded := Empty().Note([]Entry{entry(first, "tools/rg@14.1.0", "https://example.test/rg.tgz", "rg.tgz")}, early)
	object, held := recorded.Objects[first]
	if !held {
		t.Fatalf("catalog = %#v, want a record for the object", recorded)
	}
	if object.Size != 12 || !object.FirstSeen.Equal(early) || !object.LastSeen.Equal(early) {
		t.Fatalf("object = %#v, want the size and both times recorded", object)
	}
	source := object.Sources[coord.MustParse("tools/rg@14.1.0")]
	if source.URL != "https://example.test/rg.tgz" || source.Filename != "rg.tgz" {
		t.Fatalf("source = %#v, want the URL and file name", source)
	}
}

// TestNoteRecordsASecondCoordinateForTheSameObject is the case a flat record collapses:
// two coordinates can resolve to one set of bytes, and each has its own account of them.
func TestNoteRecordsASecondCoordinateForTheSameObject(t *testing.T) {
	recorded := Empty().
		Note([]Entry{entry(first, "tools/rg@14.1.0", "https://one.test/a", "a.bin")}, early).
		Note([]Entry{entry(first, "other/rg@14.1.0", "https://two.test/b", "b.bin")}, later)
	object := recorded.Objects[first]
	if !slices.Equal(object.Coordinates(), []string{"other/rg@14.1.0", "tools/rg@14.1.0"}) {
		t.Fatalf("coordinates = %v, want both in name order", object.Coordinates())
	}
	if len(recorded.Objects) != 1 {
		t.Fatalf("catalog = %#v, want one object", recorded)
	}
	// The newest account is the one to call the object by.
	newest, found := object.Newest()
	if !found || newest.Filename != "b.bin" {
		t.Fatalf("newest = %#v, want the most recently seen source", newest)
	}
}

// TestNoteKeepsFirstSeenAndAdvancesLastSeen is what makes a record say how long DAC has held
// these bytes. A re-download is not a new object and must not reset that.
func TestNoteKeepsFirstSeenAndAdvancesLastSeen(t *testing.T) {
	name := "tools/rg@14.1.0"
	recorded := Empty().
		Note([]Entry{entry(first, name, "https://example.test/a", "a.bin")}, early).
		Note([]Entry{entry(first, name, "https://example.test/a", "a.bin")}, later)
	object := recorded.Objects[first]
	if !object.FirstSeen.Equal(early) || !object.LastSeen.Equal(later) {
		t.Fatalf("object = %#v, want the first time kept and the last time moved", object)
	}
	source := object.Sources[coord.MustParse(name)]
	if !source.FirstSeen.Equal(early) || !source.LastSeen.Equal(later) {
		t.Fatalf("source = %#v, want the same of its own times", source)
	}
}

// TestTwoObjectsFromOneCoordinateKeepTheirOwnTimes is the drift case: a publisher replaces what
// a URL serves, so one coordinate has resolved to two sets of bytes. Only the per-coordinate
// time says which of the two is the stale one.
func TestTwoObjectsFromOneCoordinateKeepTheirOwnTimes(t *testing.T) {
	name := "tools/rg@14.1.0"
	url := "https://example.test/rg.tgz"
	recorded := Empty().
		Note([]Entry{entry(first, name, url, "rg.tgz")}, early).
		Note([]Entry{entry(second, name, url, "rg.tgz")}, later)
	if len(recorded.Objects) != 2 {
		t.Fatalf("catalog = %#v, want a record for each set of bytes", recorded)
	}
	stale := recorded.Objects[first].Sources[coord.MustParse(name)]
	current := recorded.Objects[second].Sources[coord.MustParse(name)]
	if !stale.LastSeen.Equal(early) || !current.LastSeen.Equal(later) {
		t.Fatalf("stale = %#v, current = %#v, want the older bytes left behind", stale, current)
	}
}

func TestForgetDropsTheWholeRecord(t *testing.T) {
	recorded := Empty().Note([]Entry{
		entry(first, "tools/rg@14.1.0", "https://example.test/a", "a.bin"),
		entry(second, "tools/jq@1.7", "https://example.test/b", "b.bin"),
	}, early)
	updated, forgotten := recorded.Forget([]string{first, digest.Bytes([]byte("never stored"))})
	if !slices.Equal(forgotten, []string{first}) {
		t.Fatalf("forgotten = %v, want only the record the catalog held", forgotten)
	}
	if _, held := updated.Objects[first]; held {
		t.Fatalf("catalog = %#v, want the record gone", updated)
	}
	if _, held := updated.Objects[second]; !held {
		t.Fatalf("catalog = %#v, want the other record kept", updated)
	}
}

// TestChangesLeaveTheCatalogTheyWereGivenAlone holds the pure-functional contract the trust list
// holds to. It matters more here, because an object carries a map of its own: a value copied
// without copying that map would let one caller write through another's record.
func TestChangesLeaveTheCatalogTheyWereGivenAlone(t *testing.T) {
	name := "tools/rg@14.1.0"
	original := Empty().Note([]Entry{entry(first, name, "https://example.test/a", "a.bin")}, early)
	original.Note([]Entry{entry(first, "other/rg@14.1.0", "https://example.test/b", "b.bin")}, later)
	original.Forget([]string{first})

	object, held := original.Objects[first]
	if !held || len(object.Sources) != 1 {
		t.Fatalf("original = %#v, want one untouched record with one source", original)
	}
	if !object.LastSeen.Equal(early) {
		t.Fatalf("object = %#v, want the original time", object)
	}
}

func TestValidateRejectsAnUnsupportedSchemaVersion(t *testing.T) {
	catalog := Empty()
	catalog.SchemaVersion = SchemaVersion + 1
	if err := catalog.Validate(); err == nil {
		t.Fatal("Validate accepted a schema version DAC does not understand")
	}
}

// TestValidateRejectsAKeyThatIsNotADigest keeps a hand-edited file from putting entries in the
// catalog that no cache object could ever match.
func TestValidateRejectsAKeyThatIsNotADigest(t *testing.T) {
	catalog := Empty()
	catalog.Objects["not-a-digest"] = Object{}
	if err := catalog.Validate(); err == nil {
		t.Fatal("Validate accepted a key that is not a digest")
	}
}
