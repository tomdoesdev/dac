package manifest

import (
	"errors"
	"strings"
	"testing"

	"github.com/tomdoesdev/dac/internal/project"
)

// TestAssetNamesAreOpaquePrintableIdentifiers documents that asset names are
// map keys, not filenames or command tokens. Punctuation, Unicode, namespace
// separators, and leading dashes are valid; whitespace, controls, invalid UTF-8,
// and the empty name are rejected because they cannot be represented safely.
func TestAssetNamesAreOpaquePrintableIdentifiers(t *testing.T) {
	valid := []string{
		// Slash and @ are common in namespaced, versioned package identifiers.
		"namespace/asset@version",
		// Scoped-package and version punctuation remains opaque to DAC.
		"@scope/pkg:1.2.3",
		// Shell metacharacters are data; command rendering must quote them later.
		"quoted'asset\"$`;",
		// A leading dash is allowed in the manifest even though the CLI needs --.
		"-leading-option",
		// Printable non-ASCII identifiers must survive without transliteration.
		"雪/📦@最新版",
	}
	for _, name := range valid {
		if !ValidAssetName(name) {
			t.Errorf("ValidAssetName(%q) = false", name)
		}
	}
	invalid := []string{
		"",                   // An empty key cannot identify an asset in messages or commands.
		"has space",          // Spaces would make copied CLI hints ambiguous.
		"has\ttab",           // Tabs are non-printing separators rather than identifier data.
		"has\nnewline",       // Newlines would corrupt diagnostics and line-oriented output.
		"control\x00",        // NUL and other controls cannot be handled safely downstream.
		string([]byte{0xff}), // Manifests and CLI output require valid UTF-8.
	}
	for _, name := range invalid {
		if ValidAssetName(name) {
			t.Errorf("ValidAssetName(%q) = true", name)
		}
	}
}

// TestManifestControlCharactersRoundTrip distinguishes free-form variable
// values from constrained asset names. TOML serialization must escape every C0
// and C1 control value losslessly because variables may legitimately contain
// opaque data even though identifiers may not.
func TestManifestControlCharactersRoundTrip(t *testing.T) {
	var controls strings.Builder
	for character := rune(0); character <= 0x9f; character++ {
		if character < 0x20 || character >= 0x7f {
			controls.WriteRune(character)
		}
	}
	value := controls.String()
	path := t.TempDir() + "/dac.toml"
	want := Manifest{Version: project.Version, Files: map[string]Asset{
		"namespace/asset@version": {
			URL:       "https://example.com/artifact.bin",
			File:      "artifact.bin",
			Variables: map[string]string{"UNUSED": value},
		},
	}}
	if err := Write(path, want); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.Files["namespace/asset@version"].Variables["UNUSED"]; got != value {
		t.Fatalf("control value changed during round trip: %q", got)
	}
}

// TestResolveRejectsUnicodeEquivalentFilenames prevents two assets from
// targeting names that common filesystems treat as the same entry. Catching
// collisions while resolving the manifest avoids platform-dependent overwrite
// behavior during lock and pull.
func TestResolveRejectsUnicodeEquivalentFilenames(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
	}{
		// Composed and decomposed accents compare differently byte-for-byte but
		// normalize to the same filename on filesystems such as default macOS APFS.
		{name: "normalization", left: "é.bin", right: "e\u0301.bin"},
		// Unicode-aware case folding catches more than ASCII case-only collisions;
		// in particular, German sharp-s expands to "ss" when folded.
		{name: "case folding", left: "Straße.bin", right: "STRASSE.bin"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := Manifest{Version: project.Version, Files: map[string]Asset{
				"first":  {URL: "https://example.com/first", File: test.left},
				"second": {URL: "https://example.com/second", File: test.right},
			}}
			if _, err := Resolve(value); !errors.Is(err, ErrResolvedFileConflict) {
				t.Fatalf("Resolve error = %v, want ErrResolvedFileConflict for %q and %q", err, test.left, test.right)
			}
		})
	}
}
