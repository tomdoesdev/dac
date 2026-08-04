package coord_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tomdoesdev/dac/internal/coord"
)

func TestParseAcceptsACompleteCoordinate(t *testing.T) {
	for _, text := range []string{
		"app/geo@1",
		"backend-app/geo-database@2026.08",
		"a/b@1.0.0-RC1+build.5",
		"app_1/geo.db@1",
	} {
		value, err := coord.Parse(text)
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		if value.String() != text {
			t.Fatalf("%q round tripped as %q", text, value)
		}
	}
}

func TestParseRejectsAnythingElse(t *testing.T) {
	// Each of these is a way of saying an asset that DAC would have to guess
	// at, and guessing is what the coordinate exists to stop.
	for _, text := range []string{
		"",
		"geo@1",          // no namespace
		"app/geo",        // no version
		"app/geo@",       // empty version
		"/geo@1",         // empty namespace
		"app//geo@1",     // two separators
		"app/geo@1@2",    // two versions
		"a@b/c",          // separators out of order
		"App/geo@1",      // uppercase label
		"app/Geo@1",      // uppercase label
		"app/-geo@1",     // leading separator
		"app/geo-@1",     // trailing separator
		"app/geo@.1",     // leading separator
		"app/ geo@1",     // space
		"app/geo@1 ",     // space
		"app/geo/more@1", // nested namespace
		"app/g eo@1",     // space
		strings.Repeat("a", 65) + "/geo@1",
	} {
		if value, err := coord.Parse(text); err == nil {
			t.Fatalf("%q parsed as %q", text, value)
		}
	}
}

// A label that reads as a flag or a path segment wherever a coordinate is a
// command argument gets rejected at the edge, not discovered later.
func TestParseErrorsNameTheProblem(t *testing.T) {
	_, err := coord.Parse("app/Geo@1")
	if err == nil || !strings.Contains(err.Error(), "lowercase") {
		t.Fatalf("the error does not say what a name may contain: %v", err)
	}
}

func TestGroupSeparatesVersionsFromAssets(t *testing.T) {
	first := coord.MustParse("app/geo@1")
	second := coord.MustParse("app/geo@2")
	other := coord.MustParse("web/geo@1")
	if first.Group() != second.Group() {
		t.Fatal("two versions of one asset are in different groups")
	}
	if first.Group() == other.Group() {
		t.Fatal("a namespace does not separate two assets that share a name")
	}
	if first.Group().String() != "app/geo" {
		t.Fatalf("unexpected group %q", first.Group())
	}
}

func TestParseGroupRejectsAVersion(t *testing.T) {
	if _, err := coord.ParseGroup("app/geo"); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"app/geo@1", "geo", "app/geo/more"} {
		if _, err := coord.ParseGroup(text); err == nil {
			t.Fatalf("%q parsed as an asset", text)
		}
	}
}

// A coordinate is a map key in both project files, so it has to survive the
// round trip through JSON that every command makes.
func TestCoordinateKeysRoundTripThroughJSON(t *testing.T) {
	original := map[coord.Coordinate]int{
		coord.MustParse("web/geo@1"): 2,
		coord.MustParse("app/geo@1"): 1,
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	// Keys marshal in sorted order, which is what keeps a manifest digest
	// stable across runs and a lock file diff readable.
	if string(data) != `{"app/geo@1":1,"web/geo@1":2}` {
		t.Fatalf("unexpected encoding %s", data)
	}
	var decoded map[coord.Coordinate]int
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 2 || decoded[coord.MustParse("app/geo@1")] != 1 {
		t.Fatalf("unexpected decoding %#v", decoded)
	}
}

// An invalid key must fail at the read that found it rather than at whichever
// command first tried to use it.
func TestInvalidKeysFailToDecode(t *testing.T) {
	var decoded map[coord.Coordinate]int
	if err := json.Unmarshal([]byte(`{"geo@1":1}`), &decoded); err == nil {
		t.Fatal("a namespaceless key decoded")
	}
}

// Marshalling validates too, so a coordinate built in memory cannot put a key
// into a file that DAC would refuse to read back.
func TestInvalidCoordinatesFailToEncode(t *testing.T) {
	if _, err := json.Marshal(map[coord.Coordinate]int{{Name: "geo", Version: "1"}: 1}); err == nil {
		t.Fatal("a namespaceless coordinate encoded")
	}
}

func TestInGroupReturnsOneAssetsVersionsInOrder(t *testing.T) {
	assets := map[coord.Coordinate]int{
		coord.MustParse("app/geo@2"):   0,
		coord.MustParse("app/geo@1"):   0,
		coord.MustParse("app/other@1"): 0,
		coord.MustParse("web/geo@1"):   0,
	}
	found := coord.Versions(coord.InGroup(assets, coord.Group{Namespace: "app", Name: "geo"}))
	if len(found) != 2 || found[0] != "1" || found[1] != "2" {
		t.Fatalf("unexpected versions %#v", found)
	}
}
