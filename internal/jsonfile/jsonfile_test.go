package jsonfile

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

type document struct {
	Name  string         `json:"name"`
	Count int            `json:"count"`
	Inner map[string]any `json:"inner,omitempty"`
}

func TestDecodeStrictRejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"duplicate key", `{"name":"a","name":"b"}`, "duplicate"},
		{"nested duplicate key", `{"inner":{"x":1,"x":2}}`, "duplicate"},
		{"unknown field", `{"name":"a","extra":1}`, "unknown field"},
		{"trailing value", `{"name":"a"} {"name":"b"}`, "more than one value"},
		{"not an object", `[1,2]`, "cannot unmarshal"},
		{"truncated", `{"name":`, "EOF"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var value document
			err := DecodeStrict([]byte(testCase.data), &value)
			if err == nil {
				t.Fatalf("%q was accepted", testCase.data)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not mention %q", err, testCase.want)
			}
		})
	}
}

func TestDecodeStrictAcceptsValidInput(t *testing.T) {
	var value document
	if err := DecodeStrict([]byte(`{"name":"a","count":2,"inner":{"x":1,"y":[1,2,{"z":3}]}}`), &value); err != nil {
		t.Fatal(err)
	}
	if value.Name != "a" || value.Count != 2 {
		t.Fatalf("unexpected value: %+v", value)
	}
	// Duplicate detection must not reject repeated keys in sibling objects.
	if err := DecodeStrict([]byte(`{"inner":{"a":{"x":1},"b":{"x":2}}}`), &value); err != nil {
		t.Fatalf("sibling objects with the same key were rejected: %v", err)
	}
}

func TestReadStrictReportsMissingFiles(t *testing.T) {
	var value document
	err := ReadStrict(filepath.Join(t.TempDir(), "absent.json"), &value)
	if !os.IsNotExist(err) {
		t.Fatalf("missing file returned %v", err)
	}
}

func TestWriteAtomicReplacesAndSetsMode(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "nested", "deeper")
	path := filepath.Join(directory, "file.json")
	if err := WriteAtomic(path, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode is %o", info.Mode().Perm())
	}
	if err := WriteAtomic(path, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "second\n" {
		t.Fatalf("file holds %q: %v", data, err)
	}

	// A completed write must leave no temporary files behind.
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries", len(entries))
	}
}

func TestConcurrentWritesLeaveOneCompleteFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.json")
	contents := []string{"aaaa\n", "bbbb\n", "cccc\n", "dddd\n"}
	var group sync.WaitGroup
	for _, content := range contents {
		group.Go(func() {
			if err := WriteAtomic(path, []byte(content), 0o644); err != nil {
				t.Errorf("WriteAtomic returned %v", err)
			}
		})
	}
	group.Wait()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The rename is atomic, so a reader sees one writer's bytes, never a blend.
	if !slices.Contains(contents, string(data)) {
		t.Fatalf("file holds a partial write: %q", data)
	}
}
