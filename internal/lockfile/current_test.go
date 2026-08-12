package lockfile

import (
	"errors"
	"strings"
	"testing"

	"github.com/tomdoesdev/dac/internal/manifest"
)

// TestHintQuotesOpaqueNames ensures recovery instructions remain safe to paste
// into a POSIX shell. Asset names are deliberately opaque and may begin with a
// dash or contain shell metacharacters, so the hint must quote rather than
// reinterpret them as options or shell syntax.
func TestHintQuotesOpaqueNames(t *testing.T) {
	if got, want := hint("-scope/pkg'$`"), `run: dac lock -- '-scope/pkg'"'"'$`+"`'"; got != want {
		t.Fatalf("hint = %q, want %q", got, want)
	}
}

// TestValidateCurrentRejectsChangedResolution prevents a lock from silently
// authorizing a different download. A manifest-variable change that resolves
// to a new URL makes the lock stale even when the asset name and output file
// have not changed.
func TestValidateCurrentRejectsChangedResolution(t *testing.T) {
	resolved := []manifest.ResolvedAsset{{Name: "artifact", ResolvedURL: "https://example.com/new", ResolvedFile: "artifact.bin"}}
	lock := Lockfile{Version: Version, Files: map[string]Asset{
		"artifact": {
			ResolvedURL: "https://example.com/old", ResolvedFile: "artifact.bin",
			ConfigurationDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Digest:              "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}}
	if err := ValidateCurrent(resolved, lock); !errors.Is(err, ErrStale) {
		t.Fatalf("ValidateCurrent error = %v, want ErrStale", err)
	}
}

func TestValidateCurrentRejectsPolicyChangeAndPinMismatch(t *testing.T) {
	value := manifest.Manifest{Version: manifest.Version, Files: map[string]manifest.Asset{
		"artifact": {
			URL: "https://example.com/artifact", File: "artifact.bin",
			Pin: "sha256:" + strings.Repeat("a", 64), Variables: map[string]string{"UNUSED": "one"},
		},
	}}
	resolved, err := manifest.Resolve(value)
	if err != nil {
		t.Fatal(err)
	}
	locked := Asset{
		ResolvedURL: resolved[0].ResolvedURL, ResolvedFile: resolved[0].ResolvedFile,
		ConfigurationDigest: ConfigurationDigest(resolved[0]), Digest: "sha256:" + strings.Repeat("b", 64),
	}
	lock := Lockfile{Version: Version, Files: map[string]Asset{"artifact": locked}}
	if err := ValidateCurrent(resolved, lock); !errors.Is(err, ErrStale) {
		t.Fatalf("pin mismatch error = %v, want ErrStale", err)
	}

	locked.Digest = resolved[0].Pin
	lock.Files["artifact"] = locked
	value.Files["artifact"] = manifest.Asset{
		URL: "https://example.com/artifact", File: "artifact.bin",
		Pin: resolved[0].Pin, Variables: map[string]string{"UNUSED": "two"},
	}
	changed, err := manifest.Resolve(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrent(changed, lock); !errors.Is(err, ErrStale) {
		t.Fatalf("unused-variable change error = %v, want ErrStale", err)
	}
}
