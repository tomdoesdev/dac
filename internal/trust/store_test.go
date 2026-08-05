package trust

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/proclock"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	return New(filepath.Join(t.TempDir(), FileName))
}

// TestLoadTreatsAMissingFileAsTrustingNothing keeps the first run from failing
// before it can say the thing worth saying, which is which host to trust.
func TestLoadTreatsAMissingFileAsTrustingNothing(t *testing.T) {
	list, err := testStore(t).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(list.Hosts) != 0 || list.SchemaVersion != SchemaVersion {
		t.Fatalf("list = %+v, want an empty current-version list", list)
	}
}

func TestUpdateWritesAFileDACCanReadBack(t *testing.T) {
	store := testStore(t)
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	written, err := store.Update(context.Background(), func(list List) (List, error) {
		updated, _ := list.Add([]string{"beta.example", "alpha.example"}, now)
		return updated, nil
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !slices.Equal(written.Sorted(), []string{"alpha.example", "beta.example"}) {
		t.Fatalf("written = %v, want both hosts", written.Sorted())
	}
	data, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(data)
	if !strings.HasSuffix(text, "}\n") || !strings.Contains(text, "\n  \"hosts\"") {
		t.Fatalf("file is %q, want indented JSON ending in a newline", text)
	}
	// encoding/json writes map keys in order, so the file reads the way the list does.
	if strings.Index(text, "alpha.example") > strings.Index(text, "beta.example") {
		t.Fatalf("file is %q, want the hosts in name order", text)
	}
	info, err := os.Stat(store.Path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != fileMode {
		t.Fatalf("mode = %v, want %v", info.Mode().Perm(), os.FileMode(fileMode))
	}
	loaded, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !loaded.Trusted("alpha.example") || loaded.Hosts["alpha.example"].AddedAt != now {
		t.Fatalf("loaded = %+v, want the hosts and times that were written", loaded)
	}
}

// TestUpdateWaitsForAnotherProcessHoldingTheFile covers the read-modify-write
// race: without the lock, two runs adding one host each would keep one host.
func TestUpdateWaitsForAnotherProcessHoldingTheFile(t *testing.T) {
	store := testStore(t)
	held, err := proclock.Acquire(context.Background(), store.Path+".lock")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := store.Update(ctx, func(list List) (List, error) { return list, nil }); err == nil {
		t.Fatal("Update wrote the file while another process held it")
	}
	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := store.Update(context.Background(), func(list List) (List, error) {
		updated, _ := list.Add([]string{"example.com"}, time.Now())
		return updated, nil
	}); err != nil {
		t.Fatalf("Update after release: %v", err)
	}
}

// TestUpdateLeavesTheFileAloneWhenTheChangeFails matters because the change runs
// under the lock, and a half-applied trust list is worse than an unchanged one.
func TestUpdateLeavesTheFileAloneWhenTheChangeFails(t *testing.T) {
	store := testStore(t)
	if _, err := store.Update(context.Background(), func(list List) (List, error) {
		updated, _ := list.Add([]string{"example.com"}, time.Now())
		return updated, nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	before, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if _, err := store.Update(context.Background(), func(List) (List, error) {
		return List{}, os.ErrInvalid
	}); err == nil {
		t.Fatal("Update reported success for a change that failed")
	}
	after, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("file is %q, want the unchanged %q", after, before)
	}
}

func TestLoadRefusesAFileDACCannotTrust(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{name: "not JSON", content: "trust everything\n"},
		{name: "future schema", content: `{"schemaVersion":99,"hosts":{}}`},
		{name: "unknown field", content: `{"schemaVersion":1,"hosts":{},"allowAll":true}`},
		{name: "unknown entry field", content: `{"schemaVersion":1,"hosts":{"example.com":{"addedAt":"2026-08-05T00:00:00Z","why":"x"}}}`},
		// A duplicate key is one host stated twice, and JSON would silently keep the last.
		{name: "duplicate host", content: `{"schemaVersion":1,"hosts":{"example.com":{},"example.com":{}}}`},
		{name: "no hosts object", content: `{"schemaVersion":1}`},
		{name: "one host named twice", content: `{"schemaVersion":1,"hosts":{"Example.com":{},"example.com":{}}}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			store := testStore(t)
			if err := os.WriteFile(store.Path, []byte(test.content), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if _, err := store.Load(); err == nil {
				t.Fatalf("Load accepted %s", test.name)
			}
		})
	}
}

// TestLoadNormalizesAHandEditedHost lets somebody add a host by editing the file
// without having to know the matching rule DAC applies to it.
func TestLoadNormalizesAHandEditedHost(t *testing.T) {
	store := testStore(t)
	if err := os.WriteFile(store.Path,
		[]byte(`{"schemaVersion":1,"hosts":{"GitHub.com:443":{"addedAt":"2026-08-05T00:00:00Z","lastUsed":"0001-01-01T00:00:00Z"}}}`),
		0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	list, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !list.Trusted("github.com") {
		t.Fatalf("hosts = %v, want github.com", list.Hosts)
	}
}

func TestResolvePathPrefersTheOptionThenTheDataHomeThenHome(t *testing.T) {
	home := t.TempDir()
	data := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", data)

	explicit := filepath.Join(t.TempDir(), "hosts.json")
	if path, err := ResolvePath(explicit); err != nil || path != explicit {
		t.Fatalf("ResolvePath(explicit) = %q, %v, want %q", path, err, explicit)
	}
	want := filepath.Join(data, DirName, FileName)
	if path, err := ResolvePath(""); err != nil || path != want {
		t.Fatalf("ResolvePath = %q, %v, want %q", path, err, want)
	}
	// A relative XDG value is ignored so a build cannot trust hosts from its work directory.
	t.Setenv("XDG_DATA_HOME", "relative/share")
	want = filepath.Join(home, ".local", "share", DirName, FileName)
	if path, err := ResolvePath(""); err != nil || path != want {
		t.Fatalf("ResolvePath with a relative data home = %q, %v, want %q", path, err, want)
	}
}
