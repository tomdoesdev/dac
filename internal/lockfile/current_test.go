package lockfile

import (
	"testing"

	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/project"
)

// TestHintQuotesOpaqueNames ensures recovery instructions remain safe to paste
// into a POSIX shell. Asset names are deliberately opaque and may begin with a
// dash or contain shell metacharacters, so the hint must quote rather than
// reinterpret them as options or shell syntax.
func TestHintQuotesOpaqueNames(t *testing.T) {
	if got, want := Hint("-scope/pkg'$`"), `run: dac lock -- '-scope/pkg'"'"'$`+"`'"; got != want {
		t.Fatalf("Hint = %q, want %q", got, want)
	}
}

// TestValidateCurrentRejectsChangedResolution prevents a lock from silently
// authorizing a different download. A manifest-variable change that resolves
// to a new URL makes the lock stale even when the asset name and output file
// have not changed.
func TestValidateCurrentRejectsChangedResolution(t *testing.T) {
	resolved := []manifest.ResolvedAsset{{Name: "artifact", ResolvedURL: "https://example.com/new", ResolvedFile: "artifact.bin"}}
	lock := Lockfile{Version: project.Version, Files: map[string]Asset{
		"artifact": {ResolvedURL: "https://example.com/old", ResolvedFile: "artifact.bin", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}}
	if err := ValidateCurrent(resolved, lock); err == nil {
		t.Fatal("ValidateCurrent accepted a changed URL")
	}
}
