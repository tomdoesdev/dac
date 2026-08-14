package lockfile

import (
	"errors"
	"strings"
	"testing"

	"github.com/tomdoesdev/dac/internal/manifest"
)

// TestUpdateRecoveryCarriesNameStructurally keeps an opaque asset name out of
// any command text this package builds. It names the asset as data so the
// renderer can quote it; rendering it here would put shell syntax in the
// package that owns accepted state.
func TestUpdateRecoveryCarriesNameStructurally(t *testing.T) {
	const name = "-scope/pkg'$`"
	recovery := UpdateRecovery(name)
	if recovery.Command != "pull" || len(recovery.Flags) != 1 || recovery.Flags[0] != "--update-lockfile" {
		t.Fatalf("targeted recovery = %+v, want pull --update-lockfile", recovery)
	}
	if len(recovery.Assets) != 1 || recovery.Assets[0] != name {
		t.Fatalf("recovery assets = %q, want the name verbatim", recovery.Assets)
	}
	if whole := UpdateRecovery(); whole.Command != "pull" || len(whole.Assets) != 0 || len(whole.Flags) != 1 {
		t.Fatalf("whole-lock recovery = %+v, want pull --update-lockfile", whole)
	}
}

// TestValidateCurrentRejectsChangedResolution prevents a lock from silently
// authorizing a different download. A manifest-variable change that resolves
// to a new URL makes the lock stale even when the asset name and output file
// have not changed.
func TestValidateCurrentRejectsChangedResolution(t *testing.T) {
	resolved, err := manifest.Resolve(manifest.Manifest{Version: manifest.Version, Files: map[string]manifest.Asset{
		"artifact": {URL: "https://example.com/new", File: "artifact.bin"},
	}})
	if err != nil {
		t.Fatal(err)
	}
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
		ResolvedURL: first(t, resolved).ResolvedURL, ResolvedFile: first(t, resolved).ResolvedFile,
		ConfigurationDigest: ConfigurationDigest(first(t, resolved)), Digest: "sha256:" + strings.Repeat("b", 64),
	}
	lock := Lockfile{Version: Version, Files: map[string]Asset{"artifact": locked}}
	if err := ValidateCurrent(resolved, lock); !errors.Is(err, ErrStale) {
		t.Fatalf("pin mismatch error = %v, want ErrStale", err)
	}

	locked.Digest = first(t, resolved).Pin
	lock.Files["artifact"] = locked
	value.Files["artifact"] = manifest.Asset{
		URL: "https://example.com/artifact", File: "artifact.bin",
		Pin: first(t, resolved).Pin, Variables: map[string]string{"UNUSED": "two"},
	}
	changed, err := manifest.Resolve(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrent(changed, lock); !errors.Is(err, ErrStale) {
		t.Fatalf("unused-variable change error = %v, want ErrStale", err)
	}
}

// first returns the single resolved asset a fixture declares.
func first(t *testing.T, resolution manifest.Resolution) manifest.ResolvedAsset {
	t.Helper()
	assets := resolution.All()
	if len(assets) == 0 {
		t.Fatal("resolution is empty")
	}
	return assets[0]
}
