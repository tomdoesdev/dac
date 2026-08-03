package filename

import (
	"strings"
	"testing"
)

func TestFromDispositionReadsEveryHeaderSpelling(t *testing.T) {
	cases := map[string]string{
		`attachment; filename="terraform.zip"`: "terraform.zip",
		`attachment; filename=terraform.zip`:   "terraform.zip",
		`inline; filename="notes.txt"`:         "notes.txt",
		// The RFC 5987 form, which mime.ParseMediaType decodes into the plain
		// parameter for us.
		`attachment; filename*=UTF-8''caf%C3%A9.txt`: "café.txt",
		// An encoded name beside an ASCII one: the encoded spelling wins, in
		// either order, because it is the one that carries the real name.
		`attachment; filename="fallback.txt"; filename*=UTF-8''caf%C3%A9.txt`: "café.txt",
		`attachment; filename*=UTF-8''caf%C3%A9.txt; filename="fallback.txt"`: "café.txt",
		// An RFC 2231 continuation, reassembled in order.
		`attachment; filename*0="long"; filename*1="name.txt"`: "longname.txt",
	}
	for header, want := range cases {
		if got := FromDisposition(header); got != want {
			t.Fatalf("%q gave %q, want %q", header, got, want)
		}
	}
}

func TestFromDispositionReportsNoNameRatherThanAnUnsafeOne(t *testing.T) {
	// A header that parses is not a header that names something usable. Each of
	// these is a well-formed Content-Disposition whose name DAC will not take.
	for _, header := range []string{
		"",
		"   ",
		"attachment",
		")(*&^%",
		`attachment; filename="../../etc/passwd"`,
		`attachment; filename="/etc/passwd"`,
		`attachment; filename=".."`,
		`attachment; filename="-rf"`,
	} {
		if got := FromDisposition(header); got != "" {
			t.Fatalf("%q gave %q, want no name", header, got)
		}
	}
}

func TestFromURLTakesTheLastPathSegment(t *testing.T) {
	cases := map[string]string{
		"https://example.com/terraform/1.9.0/terraform_linux.zip": "terraform_linux.zip",
		"https://example.com/geo/database.bin?token=abc&v=2":      "database.bin",
		"https://example.com/geo/database.bin#fragment":           "database.bin",
		"https://example.com/a/b/":                                "b",
		// Percent-escapes in the final segment are the origin's spelling of the
		// name, so they are decoded.
		"https://example.com/a/hello%20world.txt": "hello world.txt",
	}
	for raw, want := range cases {
		if got := FromURL(raw); got != want {
			t.Fatalf("%q gave %q, want %q", raw, got, want)
		}
	}
}

func TestFromURLReportsNoNameWhenTheURLSpellsNone(t *testing.T) {
	for _, raw := range []string{
		"",
		"https://example.com",
		"https://example.com/",
		"https://example.com/a/..",
		"https://example.com/a/.",
		// An encoded separator stays inside the segment, so the name is two
		// elements and is refused rather than silently truncated to the tail.
		"https://example.com/a/b%2Fc.txt",
		"://not a url",
	} {
		if got := FromURL(raw); got != "" {
			t.Fatalf("%q gave %q, want no name", raw, got)
		}
	}
}

func TestCleanKeepsOrdinaryNames(t *testing.T) {
	for _, name := range []string{
		"terraform_1.9.0_linux_amd64.zip",
		"database.bin",
		"café.txt",
		"a",
		".hidden",
		"name with spaces.tar.gz",
		strings.Repeat("a", maxLength),
	} {
		if got := Clean(name); got != name {
			t.Fatalf("%q became %q", name, got)
		}
	}
}

func TestCleanTrimsSurroundingSpace(t *testing.T) {
	if got := Clean("  asset.bin \t"); got != "asset.bin" {
		t.Fatalf("got %q", got)
	}
}

func TestCleanRefusesAnythingThatIsNotOnePathElement(t *testing.T) {
	cases := []string{
		"",
		"   ",
		".",
		"..",
		"a/b",
		"/etc/passwd",
		"../../etc/passwd",
		`a\b`,
		// A leading dash becomes a flag to whatever the calling script runs next.
		"-rf",
		"--output=/etc/passwd",
		// Control bytes, including the C1 range a Latin-1 header can carry.
		"a\x00b",
		"a\nb",
		"a\tb",
		"a\x7fb",
		"a\u009fb",
		strings.Repeat("a", maxLength+1),
	}
	for _, name := range cases {
		if got := Clean(name); got != "" {
			t.Fatalf("%q was accepted as %q", name, got)
		}
	}
}

// TestCleanIsIdempotent is what lets a lock file be validated by comparing an
// entry against Clean of itself.
func TestCleanIsIdempotent(t *testing.T) {
	for _, name := range []string{"asset.bin", "  asset.bin  ", "..", "a/b", ""} {
		once := Clean(name)
		if twice := Clean(once); twice != once {
			t.Fatalf("%q cleaned to %q then to %q", name, once, twice)
		}
	}
}
