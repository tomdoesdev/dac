package filename

import (
	"strings"
	"testing"
)

// TestFromDispositionReadsEveryHeaderSpelling documents the interoperable
// Content-Disposition forms DAC accepts when deriving an artifact filename.
// The cases cover common quoting plus the internationalized and segmented forms
// emitted by real HTTP servers.
func TestFromDispositionReadsEveryHeaderSpelling(t *testing.T) {
	cases := map[string]string{
		// Quoted filename is the conventional attachment spelling.
		`attachment; filename="terraform.zip"`: "terraform.zip",
		// Token values are legal and common for punctuation-free filenames.
		`attachment; filename=terraform.zip`: "terraform.zip",
		// Inline responses can still provide a useful download name.
		`inline; filename="notes.txt"`: "notes.txt",
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

// TestFromDispositionReportsNoNameRatherThanAnUnsafeOne makes filename inference
// fail closed. An absent, malformed, path-like, or option-like server value must
// produce no suggestion so downstream code can choose an explicit safe name.
func TestFromDispositionReportsNoNameRatherThanAnUnsafeOne(t *testing.T) {
	// A header that parses is not a header that names something usable. Each of
	// these is a well-formed Content-Disposition whose name DAC will not take.
	for _, header := range []string{
		"",           // No header provides no filename evidence.
		"   ",        // Whitespace-only input is also semantically absent.
		"attachment", // A valid disposition without filename has no name.
		")(*&^%",     // Malformed media-type syntax must fail cleanly.
		`attachment; filename="../../etc/passwd"`, // Relative traversal is more than one path element.
		`attachment; filename="/etc/passwd"`,      // Absolute paths must never escape the download root.
		`attachment; filename=".."`,               // A parent-directory pseudo-name is not a file.
		`attachment; filename="-rf"`,              // Leading dashes can become options in later tooling.
	} {
		if got := FromDisposition(header); got != "" {
			t.Fatalf("%q gave %q, want no name", header, got)
		}
	}
}

// TestFromURLTakesTheLastPathSegment defines URL fallback inference. Only path
// segments contribute to filenames; credentials/query/fragment data must not,
// trailing slashes use the last non-empty segment, and escaped filename bytes
// are decoded before safety validation.
func TestFromURLTakesTheLastPathSegment(t *testing.T) {
	cases := map[string]string{
		// A normal download URL yields its final artifact segment.
		"https://example.com/terraform/1.9.0/terraform_linux.zip": "terraform_linux.zip",
		// Query parameters often contain secrets and are never part of the filename.
		"https://example.com/geo/database.bin?token=abc&v=2": "database.bin",
		// Fragments are client-side selectors and likewise do not name the file.
		"https://example.com/geo/database.bin#fragment": "database.bin",
		// Directory-looking URLs fall back to their last non-empty segment.
		"https://example.com/a/b/": "b",
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

// TestFromURLReportsNoNameWhenTheURLSpellsNone covers empty, structural, unsafe,
// and malformed URL inputs. Inference must return no name rather than inventing
// one or truncating an unsafe segment.
func TestFromURLReportsNoNameWhenTheURLSpellsNone(t *testing.T) {
	for _, raw := range []string{
		"",                         // Empty input contains no path.
		"https://example.com",      // A host alone contains no filename segment.
		"https://example.com/",     // A root slash is structural, not a name.
		"https://example.com/a/..", // Parent traversal resolves to a directory.
		"https://example.com/a/.",  // Current-directory syntax does not name a file.
		// An encoded separator stays inside the segment, so the name is two
		// elements and is refused rather than silently truncated to the tail.
		"https://example.com/a/b%2Fc.txt",
		"://not a url", // Malformed URLs must fail without leaking parser artifacts.
	} {
		if got := FromURL(raw); got != "" {
			t.Fatalf("%q gave %q, want no name", raw, got)
		}
	}
}

// TestCleanKeepsOrdinaryNames protects against over-sanitization. Safe artifact
// names—including Unicode, dotfiles, internal spaces, and the maximum supported
// length—must survive byte-for-byte so DAC does not rename upstream artifacts.
func TestCleanKeepsOrdinaryNames(t *testing.T) {
	for _, name := range []string{
		"terraform_1.9.0_linux_amd64.zip", // Typical release artifact punctuation.
		"database.bin",                    // Simple baseline filename.
		"café.txt",                        // Valid Unicode must not be transliterated.
		"a",                               // The smallest non-empty name is valid.
		".hidden",                         // A dotfile is a file, unlike "." or "..".
		"name with spaces.tar.gz",         // Internal spaces are safe path-element data.
		strings.Repeat("a", maxLength),    // The documented length boundary is inclusive.
	} {
		if got := Clean(name); got != name {
			t.Fatalf("%q became %q", name, got)
		}
	}
}

// TestCleanTrimsSurroundingSpace permits human/server formatting whitespace
// without preserving invisible suffixes that make filenames hard to address.
func TestCleanTrimsSurroundingSpace(t *testing.T) {
	if got := Clean("  asset.bin \t"); got != "asset.bin" {
		t.Fatalf("got %q", got)
	}
}

// TestCleanRefusesAnythingThatIsNotOnePathElement is the core filename safety
// boundary. DAC writes inferred names beneath a project directory, so empty,
// traversal, option-like, control-bearing, or overlong inputs must be rejected
// rather than modified into a potentially surprising destination.
func TestCleanRefusesAnythingThatIsNotOnePathElement(t *testing.T) {
	cases := []string{
		"",                 // There is no path element to install.
		"   ",              // Trimming cannot turn whitespace into a name.
		".",                // Current-directory syntax is not a file name.
		"..",               // Parent-directory syntax would escape the destination.
		"a/b",              // POSIX separators create multiple path elements.
		"/etc/passwd",      // Absolute paths escape the download directory.
		"../../etc/passwd", // Relative traversal escapes after path cleaning.
		`a\b`,              // Backslashes are separators on Windows and unsafe cross-platform.
		// A leading dash becomes a flag to whatever the calling script runs next.
		"-rf",
		"--output=/etc/passwd",
		// Control bytes, including the C1 range a Latin-1 header can carry.
		"a\x00b",
		"a\nb",
		"a\tb",
		"a\x7fb",
		"a\u009fb",
		strings.Repeat("a", maxLength+1), // Reject just beyond the inclusive size boundary.
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
	for _, name := range []string{
		"asset.bin",     // An already-safe name remains stable.
		"  asset.bin  ", // A normalizable name stabilizes after trimming.
		"..",            // A rejected traversal stabilizes at the empty sentinel.
		"a/b",           // A rejected multi-element path also stabilizes as empty.
		"",              // The empty sentinel is itself stable.
	} {
		once := Clean(name)
		if twice := Clean(once); twice != once {
			t.Fatalf("%q cleaned to %q then to %q", name, once, twice)
		}
	}
}
