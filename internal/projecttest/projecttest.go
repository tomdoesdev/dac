// Package projecttest holds project file helpers shared by the tests of more
// than one package. It exists because the CLI and application test packages
// both need to assert the same invariant: a manifest and its lock agree.
package projecttest

import (
	"os"
	"testing"

	"github.com/tomdoesdev/dac/internal/project"
)

// Check reads both project files and fails the test unless they agree.
func Check(t *testing.T, manifestPath, lockPath string) (project.Manifest, project.Lock) {
	t.Helper()
	manifest, err := project.ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := project.ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := project.CheckLock(manifest, lock); err != nil {
		t.Fatal(err)
	}
	return manifest, lock
}

// MustRead returns a file's bytes or fails the test.
func MustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
