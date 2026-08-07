package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadManifestRejectsTheRemovedHTTPOptIn keeps legacy policy settings from
// silently surviving as fields DAC no longer understands.
func TestReadManifestRejectsTheRemovedHTTPOptIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dac.json")
	data := []byte(`{"schemaVersion":2,"assets":{"app/geo@1":{"url":"http://example.com/geo.bin","allowInsecureHttp":true}}}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(path); err == nil || !strings.Contains(err.Error(), "allowInsecureHttp") {
		t.Fatalf("legacy HTTP opt-in error = %v", err)
	}
}
