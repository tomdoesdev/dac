package getit

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// This package is meant to leave here one day, the way internal/fs is. What
// makes that a move rather than a rewrite is that it imports nothing but the
// standard library, so this is the constraint the rest of the design exists to
// hold.
//
// Direct imports are the whole check. A DAC package can only be reached through
// a DAC import, so a package with none directly has none at all.
func TestThePackageImportsOnlyTheStandardLibrary(t *testing.T) {
	const project = "github.com/tomdoesdev/dac"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}
	fileSet := token.NewFileSet()
	checked := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		// The tests come along when the package moves, so they are held to this too.
		file, err := parser.ParseFile(fileSet, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		checked++
		for _, imported := range file.Imports {
			path, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				t.Fatalf("%s: reading the import %s: %v", name, imported.Path.Value, err)
			}
			if strings.HasPrefix(path, project) {
				t.Errorf("%s imports %s, which this package cannot take with it", name, path)
			}
			// A first element with a dot in it is a host, so the import is
			// somebody else's module rather than the standard library.
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %s, which is outside the standard library", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no Go files were found to check")
	}
}
