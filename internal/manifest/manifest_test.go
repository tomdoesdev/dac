package manifest

import (
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/asset"
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
	want := Manifest{Version: Version, Files: map[string]Asset{
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
			value := Manifest{Version: Version, Files: map[string]Asset{
				"first":  {URL: "https://example.com/first", File: test.left},
				"second": {URL: "https://example.com/second", File: test.right},
			}}
			if _, err := Resolve(value); !errors.Is(err, ErrResolvedFileConflict) {
				t.Fatalf("Resolve error = %v, want ErrResolvedFileConflict for %q and %q", err, test.left, test.right)
			}
		})
	}
}

// TestResolveCarriesParsedTransferLimits pins the transfer policy to the
// manifest's declared limits. Resolve once re-parsed these strings and
// discarded the error, so a grammar the parser rejected silently produced a
// zero policy, which disables the limit rather than enforcing it.
func TestResolveCarriesParsedTransferLimits(t *testing.T) {
	value := Manifest{Version: Version, Files: map[string]Asset{
		"declared": {URL: "https://example.com/a", File: "a.bin", MaxSize: "2MiB", IdleTimeout: "45s"},
		"default":  {URL: "https://example.com/b", File: "b.bin"},
	}}
	resolved, err := Resolve(value)
	if err != nil {
		t.Fatal(err)
	}
	declared, ok := resolved.Get("declared")
	if !ok {
		t.Fatal("Resolve dropped the declared asset")
	}
	if got, want := declared.Transfer.MaxSize, int64(2<<20); got != want {
		t.Errorf("declared MaxSize = %d, want %d", got, want)
	}
	if got, want := declared.Transfer.IdleTimeout, 45*time.Second; got != want {
		t.Errorf("declared IdleTimeout = %v, want %v", got, want)
	}
	fallback, _ := resolved.Get("default")
	if got, want := fallback.Transfer, asset.DefaultTransferPolicy(); got != want {
		t.Errorf("default Transfer = %+v, want %+v", got, want)
	}
}

// TestValidateRejectsMalformedTransferLimits keeps an unparseable limit a
// configuration failure rather than a silently unlimited transfer.
func TestValidateRejectsMalformedTransferLimits(t *testing.T) {
	tests := []struct {
		name  string
		asset Asset
		want  error
	}{
		{name: "max size", asset: Asset{URL: "https://example.com/a", File: "a.bin", MaxSize: "2 furlongs"}, want: ErrInvalidMaxSize},
		{name: "idle timeout", asset: Asset{URL: "https://example.com/a", File: "a.bin", IdleTimeout: "-5s"}, want: ErrInvalidIdleTimeout},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			value := Manifest{Version: Version, Files: map[string]Asset{"only": test.asset}}
			if err := Validate(value); !errors.Is(err, test.want) {
				t.Fatalf("Validate error = %v, want %v", err, test.want)
			}
			if _, err := Resolve(value); !errors.Is(err, test.want) {
				t.Fatalf("Resolve error = %v, want %v", err, test.want)
			}
		})
	}
}

// TestGlobalsResolveThroughTheirOwnNamespace covers dac's two variable scopes
// at once. A global is shared by every asset and is addressed only as
// {{ $.KEY }}, while an asset's own variables keep the flat spelling, so
// a template always states which scope a value came from even when the two
// scopes use the same key.
func TestGlobalsResolveThroughTheirOwnNamespace(t *testing.T) {
	value := Manifest{
		Version: Version,
		Globals: map[string]string{"VERSION": "3.9.0"},
		Files: map[string]Asset{
			"shared": {URL: "https://example.com/shared-{{$.VERSION}}.bin", File: "shared-{{ $.VERSION }}.bin"},
			"both": {
				URL:       "https://example.com/both-{{$.VERSION}}-{{.VERSION}}.bin",
				File:      "both.bin",
				Variables: map[string]string{"VERSION": "local"},
			},
		},
	}
	resolved, err := Resolve(value)
	if err != nil {
		t.Fatal(err)
	}
	shared, ok := resolved.Get("shared")
	if !ok {
		t.Fatal("Resolve dropped the shared asset")
	}
	if shared.ResolvedURL != "https://example.com/shared-3.9.0.bin" || shared.ResolvedFile != "shared-3.9.0.bin" {
		t.Errorf("shared resolved to %q and %q", shared.ResolvedURL, shared.ResolvedFile)
	}
	both, _ := resolved.Get("both")
	if want := "https://example.com/both-3.9.0-local.bin"; both.ResolvedURL != want {
		t.Errorf("both ResolvedURL = %q, want %q", both.ResolvedURL, want)
	}
}

// TestGlobalsAreNotAddressableAsAssetVariables keeps the two scopes separate in
// both directions. Merging globals into the flat namespace would let a newly
// defined global silently change what an existing asset resolves to, so an
// unqualified reference must fail exactly as an undefined variable does.
func TestGlobalsAreNotAddressableAsAssetVariables(t *testing.T) {
	tests := []struct {
		name  string
		value Manifest
	}{
		{
			name: "global referenced without its namespace",
			value: Manifest{Version: Version, Globals: map[string]string{"VERSION": "3.9.0"}, Files: map[string]Asset{
				"only": {URL: "https://example.com/a-{{.VERSION}}.bin", File: "a.bin"},
			}},
		},
		{
			name: "asset variable referenced as a global",
			value: Manifest{Version: Version, Files: map[string]Asset{
				"only": {URL: "https://example.com/a-{{$.VERSION}}.bin", File: "a.bin", Variables: map[string]string{"VERSION": "3.9.0"}},
			}},
		},
		{
			name: "undefined global with no globals declared",
			value: Manifest{Version: Version, Files: map[string]Asset{
				"only": {URL: "https://example.com/a-{{$.VERSION}}.bin", File: "a.bin"},
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Resolve(test.value); !errors.Is(err, ErrRenderTemplate) {
				t.Fatalf("Resolve error = %v, want ErrRenderTemplate", err)
			}
		})
	}
}

// TestGlobalsRoundTripAndAreValidated keeps project-wide variables a first
// class part of the persisted manifest: they survive a canonical rewrite, and
// a key outside the identifier grammar is rejected before any template can
// depend on it.
func TestGlobalsRoundTripAndAreValidated(t *testing.T) {
	path := t.TempDir() + "/dac.toml"
	want := Manifest{
		Version: Version,
		Globals: map[string]string{"VERSION": "3.9.0", "CHANNEL": "stable"},
		Files: map[string]Asset{
			"artifact": {URL: "https://example.com/artifact-{{$.VERSION}}.bin", File: "artifact.bin"},
		},
	}
	if err := Write(path, want); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(loaded.Globals, want.Globals) {
		t.Fatalf("loaded globals = %#v, want %#v", loaded.Globals, want.Globals)
	}
	invalid := Manifest{Version: Version, Globals: map[string]string{"lower": "value"}, Files: map[string]Asset{}}
	if err := Validate(invalid); !errors.Is(err, ErrInvalidGlobalName) {
		t.Fatalf("Validate error = %v, want ErrInvalidGlobalName", err)
	}
}

// TestTemplateGrammarIsDirectAndDiscoverable keeps rendering and update's
// declaration checks on the same deliberately small placeholder language.
func TestTemplateGrammarIsDirectAndDiscoverable(t *testing.T) {
	file := Asset{URL: "https://example.com/{{$.RELEASE}}/{{.FLAVOUR}}/artifact.bin", File: "artifact.bin"}
	references, err := ReferencedVariables(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := references.Global["RELEASE"]; !ok {
		t.Fatalf("global references = %#v", references.Global)
	}
	if _, ok := references.Local["FLAVOUR"]; !ok {
		t.Fatalf("local references = %#v", references.Local)
	}
	invalid := []string{
		"https://example.com/{{.global.VERSION}}/artifact.bin",
		"https://example.com/{{if .VERSION}}a{{end}}/artifact.bin",
		"https://example.com/{{.VERSION | printf}}/artifact.bin",
		"https://example.com/{{.VERSION/artifact.bin",
	}
	for _, rawURL := range invalid {
		value := Manifest{Version: Version, Files: map[string]Asset{"artifact": {URL: rawURL, File: "artifact.bin"}}}
		if err := Validate(value); !errors.Is(err, ErrInvalidTemplate) {
			t.Errorf("Validate(%q) error = %v, want ErrInvalidTemplate", rawURL, err)
		}
	}
}

// TestValidateAvailableDefersOnlyMissingValues permits incremental project
// setup without weakening checks for assets that can already resolve.
func TestValidateAvailableDefersOnlyMissingValues(t *testing.T) {
	value := Manifest{Version: Version, Files: map[string]Asset{
		"pending": {URL: "https://example.com/{{$.VERSION}}/pending.bin", File: "pending.bin"},
		"ready":   {URL: "not-an-http-url", File: "ready.bin"},
	}}
	if err := ValidateAvailable(value); !errors.Is(err, ErrInvalidResolvedURL) {
		t.Fatalf("ValidateAvailable error = %v, want ErrInvalidResolvedURL", err)
	}
	value.Files["ready"] = Asset{URL: "https://example.com/ready.bin", File: "PENDING.bin"}
	if err := ValidateAvailable(value); !errors.Is(err, ErrResolvedFileConflict) {
		t.Fatalf("static collision error = %v, want ErrResolvedFileConflict", err)
	}
}

// TestManifestV1RequiresManualMigration prevents two template grammars from
// sharing one version number and being interpreted differently by old clients.
func TestManifestV1RequiresManualMigration(t *testing.T) {
	value := Manifest{Version: 1, Files: map[string]Asset{}}
	err := Validate(value)
	if !errors.Is(err, ErrUnsupportedVersion) || !strings.Contains(err.Error(), "replace {{.global.KEY}} with {{$.KEY}}") {
		t.Fatalf("v1 migration error = %v", err)
	}
}
