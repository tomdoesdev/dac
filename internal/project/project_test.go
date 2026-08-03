package project

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tom/dac/internal/coord"
	"github.com/tom/dac/internal/digest"
)

func TestWritePairRoundTrip(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	lockPath := filepath.Join(directory, "dac-lock.json")
	manifest := Manifest{
		SchemaVersion: ManifestVersion,
		Assets: map[coord.Coordinate]Asset{
			coord.MustParse("app/geo@2026.08"): {URL: "https://example.com/geo.bin"},
		},
	}
	lock, err := NewLock(manifest, map[coord.Coordinate]LockAsset{
		coord.MustParse("app/geo@2026.08"): {URL: "https://example.com/geo.bin", Digest: digest.Bytes([]byte("geo")), Size: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := WritePair(manifestPath, lockPath, manifest, lock); err != nil {
		t.Fatal(err)
	}

	readManifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	readLock, err := ReadLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckLock(readManifest, readLock); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	// The version lives in the key. A "version" field beside it would be a
	// second place to say the same thing, and the place the two could disagree.
	if strings.Contains(string(data), "\"version\"") {
		t.Fatalf("lock repeats the version outside the key: %s", data)
	}
	if !strings.Contains(string(data), "\"app/geo@2026.08\"") {
		t.Fatalf("lock is not keyed by coordinate: %s", data)
	}
}

func TestReadManifestRejectsDuplicateKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dac.json")
	data := []byte("{\"schemaVersion\":2,\"assets\":{},\"assets\":{}}")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(path); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate-key error, got %v", err)
	}
}

// A key that DAC would refuse to read back must not reach a file, so validation
// covers a manifest built in memory as well as one that was parsed.
func TestManifestRejectsInvalidCoordinates(t *testing.T) {
	for _, name := range []coord.Coordinate{
		{Name: "geo", Version: "1"},
		{Namespace: "app", Version: "1"},
		{Namespace: "app", Name: "geo"},
		{Namespace: "App", Name: "geo", Version: "1"},
		{Namespace: "app", Name: "-geo", Version: "1"},
	} {
		manifest := Manifest{
			SchemaVersion: ManifestVersion,
			Assets:        map[coord.Coordinate]Asset{name: {URL: "https://example.com/a"}},
		}
		if err := manifest.Validate(); err == nil {
			t.Fatalf("expected %#v to fail validation", name)
		}
	}
}

// A manifest written before versions were part of the key is not a manifest
// this DAC can read, and saying so is better than guessing at what it meant.
func TestReadManifestRejectsTheOlderSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dac.json")
	data := []byte(`{"schemaVersion":1,"assets":{"geo":{"version":"1","url":"https://example.com/a"}}}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(path); err == nil {
		t.Fatal("expected the older schema to be rejected")
	}
}

func TestCheckLockRejectsStaleManifest(t *testing.T) {
	manifest, lock := Empty()
	manifest.Assets[coord.MustParse("app/asset@1")] = Asset{URL: "https://example.com/a"}
	if err := CheckLock(manifest, lock); err == nil {
		t.Fatal("expected a stale lock error")
	}
}

// locked returns a manifest and a matching lock for one asset, which is the
// state every check below starts from agreeing about.
func locked(t *testing.T, asset Asset, entry LockAsset) (Manifest, Lock) {
	t.Helper()
	name := coord.MustParse("app/geo@1")
	manifest := Manifest{SchemaVersion: ManifestVersion, Assets: map[coord.Coordinate]Asset{name: asset}}
	lock, err := NewLock(manifest, map[coord.Coordinate]LockAsset{name: entry})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckLock(manifest, lock); err != nil {
		t.Fatalf("the starting state does not agree with itself: %v", err)
	}
	return manifest, lock
}

// TestCheckLockNamesEachDisagreement covers the rule every command reads the
// project through. A lock that no longer describes its manifest is the one
// failure a deployment job is supposed to stop on, and each way of reaching it
// has to say which part moved rather than "the lock file is invalid".
func TestCheckLockNamesEachDisagreement(t *testing.T) {
	value := digest.Bytes([]byte("geo"))
	name := coord.MustParse("app/geo@1")
	other := coord.MustParse("app/other@1")

	for _, testCase := range []struct {
		reason string
		change func(*Manifest, *Lock)
		want   string
	}{
		{
			reason: "the manifest changed after the lock was written",
			change: func(manifest *Manifest, _ *Lock) {
				manifest.Assets[name] = Asset{URL: "https://example.com/moved.bin"}
			},
			want: "manifest digest",
		},
		{
			reason: "the lock describes an asset the manifest dropped",
			change: func(_ *Manifest, lock *Lock) {
				lock.Assets[other] = LockAsset{URL: "https://example.com/other", Digest: digest.Bytes([]byte("other")), Size: 5}
			},
			want: "coordinates do not match",
		},
		{
			reason: "the manifest asset has no lock entry at all",
			change: func(_ *Manifest, lock *Lock) {
				delete(lock.Assets, name)
				lock.Assets[other] = LockAsset{URL: "https://example.com/other", Digest: digest.Bytes([]byte("other")), Size: 5}
			},
			want: "is not locked",
		},
		{
			reason: "the lock entry points at a different URL",
			change: func(_ *Manifest, lock *Lock) {
				entry := lock.Assets[name]
				entry.URL = "https://mirror.example.com/geo.bin"
				lock.Assets[name] = entry
			},
			want: "does not match",
		},
	} {
		manifest, lock := locked(t, Asset{URL: "https://example.com/geo.bin"}, LockAsset{URL: "https://example.com/geo.bin", Digest: value, Size: 3})
		testCase.change(&manifest, &lock)
		err := CheckLock(manifest, lock)
		if err == nil {
			t.Fatalf("%s was accepted", testCase.reason)
		}
		if !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s reported %q, which does not mention %q", testCase.reason, err, testCase.want)
		}
	}

	// A pin the lock no longer satisfies is its own reason. The URL still
	// matches, so the lock looks current until the digest is compared.
	manifest, lock := locked(t, Asset{URL: "https://example.com/geo.bin", Integrity: value},
		LockAsset{URL: "https://example.com/geo.bin", Digest: value, Size: 3})
	entry := lock.Assets[name]
	entry.Digest = digest.Bytes([]byte("other bytes"))
	lock.Assets[name] = entry
	err := CheckLock(manifest, lock)
	if err == nil || !strings.Contains(err.Error(), "integrity") {
		t.Fatalf("a lock that no longer satisfies its pin reported %v", err)
	}
}

// TestAgreesMatchesCheckLock keeps the two answers to one question together.
// Reconcile decides which assets it has to resolve with Agrees and every read
// of the project checks the whole thing with CheckLock; a lock one accepts and
// the other rejects is a project no command can settle.
func TestAgreesMatchesCheckLock(t *testing.T) {
	value := digest.Bytes([]byte("geo"))
	name := coord.MustParse("app/geo@1")
	asset := Asset{URL: "https://example.com/geo.bin"}
	entry := LockAsset{URL: "https://example.com/geo.bin", Digest: value, Size: 3}

	if !Agrees(asset, entry, true) {
		t.Fatal("a lock entry that describes its asset was reported as disagreeing")
	}
	if Agrees(asset, LockAsset{}, false) {
		t.Fatal("an asset with no lock entry was reported as agreeing")
	}
	manifest, lock := locked(t, asset, entry)
	for _, broken := range []LockAsset{
		{URL: "https://mirror.example.com/geo.bin", Digest: value, Size: 3},
		{URL: "https://example.com/geo.bin", Digest: digest.Bytes([]byte("other")), Size: 5},
	} {
		lock.Assets[name] = broken
		pinned := manifest.Assets[name]
		pinned.Integrity = value
		manifest.Assets[name] = pinned
		if Agrees(pinned, broken, true) != (CheckLock(manifest, lock) == nil) {
			t.Fatalf("Agrees and CheckLock disagree about %#v", broken)
		}
	}
}

// TestLockValidateRejectsEachBrokenField covers the invariant that lets every
// command downstream treat a Lock value as trustworthy without re-checking it.
func TestLockValidateRejectsEachBrokenField(t *testing.T) {
	value := digest.Bytes([]byte("geo"))
	name := coord.MustParse("app/geo@1")
	sound := func() Lock {
		return Lock{
			LockVersion:    LockVersion,
			ManifestDigest: digest.Bytes([]byte("manifest")),
			Assets: map[coord.Coordinate]LockAsset{
				name: {URL: "https://example.com/geo.bin", Digest: value, Size: 3},
			},
		}
	}
	if err := sound().Validate(); err != nil {
		t.Fatalf("the starting lock is not valid: %v", err)
	}
	for _, testCase := range []struct {
		reason string
		break_ func(*Lock)
		want   string
	}{
		{"an unsupported version", func(lock *Lock) { lock.LockVersion = LockVersion - 1 }, "unsupported lock version"},
		{"a manifest digest that is not one", func(lock *Lock) { lock.ManifestDigest = "sha256:xyz" }, "manifest digest"},
		{"a null asset map", func(lock *Lock) { lock.Assets = nil }, "must be an object"},
		{"a coordinate DAC would not read back", func(lock *Lock) {
			delete(lock.Assets, name)
			lock.Assets[coord.Coordinate{Namespace: "App", Name: "geo", Version: "1"}] = LockAsset{URL: "https://example.com/a", Digest: value, Size: 3}
		}, "lock asset"},
		{"an entry with no URL", func(lock *Lock) {
			lock.Assets[name] = LockAsset{Digest: value, Size: 3}
		}, "incomplete"},
		{"an entry whose digest is not one", func(lock *Lock) {
			lock.Assets[name] = LockAsset{URL: "https://example.com/a", Digest: "sha256:nope", Size: 3}
		}, "invalid digest"},
		{"a negative size", func(lock *Lock) {
			lock.Assets[name] = LockAsset{URL: "https://example.com/a", Digest: value, Size: -1}
		}, "negative size"},
		// A hand edit is the only way any of these reaches a lock file, and a
		// name is the one field here a script is invited to use as a path.
		{"a filename that escapes its directory", func(lock *Lock) {
			lock.Assets[name] = LockAsset{URL: "https://example.com/a", Digest: value, Size: 3, Filename: "../../etc/passwd"}
		}, "invalid filename"},
		{"a filename carrying a control byte", func(lock *Lock) {
			lock.Assets[name] = LockAsset{URL: "https://example.com/a", Digest: value, Size: 3, Filename: "geo\x00.bin"}
		}, "invalid filename"},
		{"a filename that is not the form DAC writes", func(lock *Lock) {
			lock.Assets[name] = LockAsset{URL: "https://example.com/a", Digest: value, Size: 3, Filename: "  geo.bin  "}
		}, "invalid filename"},
	} {
		lock := sound()
		testCase.break_(&lock)
		err := lock.Validate()
		if err == nil {
			t.Fatalf("%s was accepted", testCase.reason)
		}
		if !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s reported %q, which does not mention %q", testCase.reason, err, testCase.want)
		}
	}
}

// TestManifestValidateRejectsEachBrokenAsset covers the other half of the same
// guarantee, including the URL policy: a manifest is where a URL DAC will not
// request has to be refused, because everything after it assumes one that is.
func TestManifestValidateRejectsEachBrokenAsset(t *testing.T) {
	name := coord.MustParse("app/geo@1")
	sound := func() Manifest {
		return Manifest{
			SchemaVersion: ManifestVersion,
			Assets:        map[coord.Coordinate]Asset{name: {URL: "https://example.com/geo.bin"}},
		}
	}
	if err := sound().Validate(); err != nil {
		t.Fatalf("the starting manifest is not valid: %v", err)
	}
	for _, testCase := range []struct {
		reason string
		break_ func(*Manifest)
		want   string
	}{
		{"an unsupported schema version", func(manifest *Manifest) { manifest.SchemaVersion = ManifestVersion - 1 }, "unsupported manifest schema version"},
		{"a null asset map", func(manifest *Manifest) { manifest.Assets = nil }, "must be an object"},
		{"a relative URL", func(manifest *Manifest) { manifest.Assets[name] = Asset{URL: "/geo.bin"} }, "invalid URL"},
		{"a URL with no host", func(manifest *Manifest) { manifest.Assets[name] = Asset{URL: "https:///geo.bin"} }, "invalid URL"},
		{"a scheme DAC does not speak", func(manifest *Manifest) { manifest.Assets[name] = Asset{URL: "ftp://example.com/geo.bin"} }, "not permitted"},
		{"an insecure URL with no opt-in", func(manifest *Manifest) { manifest.Assets[name] = Asset{URL: "http://example.com/geo.bin"} }, "not permitted"},
		{"credentials in the URL", func(manifest *Manifest) {
			manifest.Assets[name] = Asset{URL: "https://user:pw@example.com/geo.bin"}
		}, "not permitted"},
		{"an integrity value that is not a digest", func(manifest *Manifest) {
			manifest.Assets[name] = Asset{URL: "https://example.com/geo.bin", Integrity: "sha256:nope"}
		}, "invalid integrity"},
	} {
		manifest := sound()
		testCase.break_(&manifest)
		err := manifest.Validate()
		if err == nil {
			t.Fatalf("%s was accepted", testCase.reason)
		}
		if !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("%s reported %q, which does not mention %q", testCase.reason, err, testCase.want)
		}
	}

	// The opt-in is what makes an insecure URL a decision rather than an
	// accident, so it has to actually permit one.
	manifest := sound()
	manifest.Assets[name] = Asset{URL: "http://example.com/geo.bin", AllowInsecureHTTP: true}
	if err := manifest.Validate(); err != nil {
		t.Fatalf("an insecure URL that opted in was refused: %v", err)
	}
}

// TestNormalizeRewritesTheSRISpelling covers the conversion that has to happen
// before the manifest digest is taken. DAC accepts the Subresource Integrity
// spelling on input and compares only the canonical one, so a manifest that
// skipped this would hash differently from the same manifest written by DAC.
func TestNormalizeRewritesTheSRISpelling(t *testing.T) {
	name := coord.MustParse("app/geo@1")
	canonical := digest.Bytes([]byte("geo"))
	sri := "sha256-" + base64.StdEncoding.EncodeToString(mustHex(t, canonical))
	manifest := Manifest{
		SchemaVersion: ManifestVersion,
		Assets:        map[coord.Coordinate]Asset{name: {URL: "https://example.com/geo.bin", Integrity: sri}},
	}
	if err := manifest.Normalize(); err != nil {
		t.Fatal(err)
	}
	if got := manifest.Assets[name].Integrity; got != canonical {
		t.Fatalf("Normalize left the integrity as %q, want %q", got, canonical)
	}
	// A second pass over a manifest DAC already wrote must not change it.
	if err := manifest.Normalize(); err != nil {
		t.Fatal(err)
	}
	if got := manifest.Assets[name].Integrity; got != canonical {
		t.Fatalf("a second Normalize rewrote the integrity to %q", got)
	}

	broken := Manifest{
		SchemaVersion: ManifestVersion,
		Assets:        map[coord.Coordinate]Asset{name: {URL: "https://example.com/geo.bin", Integrity: "sha256-not-base64!"}},
	}
	if err := broken.Normalize(); err == nil {
		t.Fatal("an integrity value that is not a digest was normalized")
	}
}

func mustHex(t *testing.T, value string) []byte {
	t.Helper()
	hexValue, err := digest.Hex(value)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(hexValue)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestReadLockIfPresentSeparatesAbsentFromInvalid covers the difference a first
// pull depends on. A lock that does not exist yet is the state pull is there to
// leave behind; a lock that exists and cannot be read is a failure, and reading
// the second as the first would have pull quietly rebuild a file somebody was
// in the middle of hand-editing.
func TestReadLockIfPresentSeparatesAbsentFromInvalid(t *testing.T) {
	directory := t.TempDir()
	lock, found, err := ReadLockIfPresent(filepath.Join(directory, "absent.json"))
	if err != nil || found {
		t.Fatalf("a missing lock returned %#v %t %v", lock, found, err)
	}

	path := filepath.Join(directory, "dac-lock.json")
	if err := os.WriteFile(path, []byte(`{"lockVersion":3,"manifestDigest":"nope","assets":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, found, err := ReadLockIfPresent(path); err == nil || found {
		t.Fatalf("an unreadable lock returned found=%t err=%v", found, err)
	}
}

// TestWritePairRestoresTheManifestWhenTheLockWriteFails covers the reason the
// two files are written by one function. A manifest that reached the disk
// without its lock is a project every later command rejects as stale, over a
// change the operator was told had failed.
func TestWritePairRestoresTheManifestWhenTheLockWriteFails(t *testing.T) {
	directory := t.TempDir()
	manifestPath := filepath.Join(directory, "dac.json")
	// A regular file where the lock's directory should be, so creating it fails
	// after the manifest has already been written.
	blocker := filepath.Join(directory, "blocked")
	if err := os.WriteFile(blocker, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(blocker, "dac-lock.json")

	name := coord.MustParse("app/geo@1")
	manifest := Manifest{
		SchemaVersion: ManifestVersion,
		Assets:        map[coord.Coordinate]Asset{name: {URL: "https://example.com/geo.bin"}},
	}
	lock, err := NewLock(manifest, map[coord.Coordinate]LockAsset{
		name: {URL: "https://example.com/geo.bin", Digest: digest.Bytes([]byte("geo")), Size: 3},
	})
	if err != nil {
		t.Fatal(err)
	}

	// With no manifest yet, a failed pair leaves none: the file this call
	// created is not a project, and keeping it would make the next command
	// report a stale lock for a change that never happened.
	if err := WritePair(manifestPath, lockPath, manifest, lock); err == nil {
		t.Fatal("a lock write into a file was reported as succeeding")
	}
	if _, err := os.Stat(manifestPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed first write left a manifest behind: %v", err)
	}

	// With a manifest already there, a failed pair leaves exactly it.
	previous := []byte(`{"schemaVersion":2,"assets":{}}` + "\n")
	if err := os.WriteFile(manifestPath, previous, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WritePair(manifestPath, lockPath, manifest, lock); err == nil {
		t.Fatal("a lock write into a file was reported as succeeding")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, previous) {
		t.Fatalf("a failed pair left the manifest as %s", data)
	}
}

// The invariant that keeps a version meaningful, enforced where a Lock value is
// built rather than only where one is written.
func TestLockRejectsTwoVersionsOfTheSameBytes(t *testing.T) {
	value := digest.Bytes([]byte("bytes"))
	manifest := Manifest{
		SchemaVersion: ManifestVersion,
		Assets: map[coord.Coordinate]Asset{
			coord.MustParse("app/geo@1"): {URL: "https://example.com/one"},
			coord.MustParse("app/geo@2"): {URL: "https://example.com/two"},
		},
	}
	_, err := NewLock(manifest, map[coord.Coordinate]LockAsset{
		coord.MustParse("app/geo@1"): {URL: "https://example.com/one", Digest: value, Size: 5},
		coord.MustParse("app/geo@2"): {URL: "https://example.com/two", Digest: value, Size: 5},
	})
	var collision *VersionCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("expected a version collision, got %v", err)
	}
	if collision.Group.String() != "app/geo" || len(collision.Versions) != 2 {
		t.Fatalf("unexpected collision: %#v", collision)
	}
}

// TestVersionCollisionErrorNamesBothVersions keeps the sentence an operator
// reads useful. The fix for a collision is to decide which of two labels was
// meant, so the message has to carry both of them and the bytes they share.
func TestVersionCollisionErrorNamesBothVersions(t *testing.T) {
	value := digest.Bytes([]byte("bytes"))
	err := &VersionCollisionError{
		Group:    coord.MustParse("app/geo@1").Group(),
		Versions: []string{"1", "2"},
		Digest:   value,
	}
	message := err.Error()
	for _, part := range []string{"app/geo", "1", "2", value} {
		if !strings.Contains(message, part) {
			t.Fatalf("the collision message %q does not name %q", message, part)
		}
	}
}

// TestManifestOrderAndCloneAreIndependent covers the two accessors every
// command builds on. Coordinates fixes the order a project is reported and
// resolved in, and Clone is what lets add stage a change without the caller's
// manifest moving underneath it.
func TestManifestOrderAndCloneAreIndependent(t *testing.T) {
	manifest := Manifest{
		SchemaVersion: ManifestVersion,
		Assets: map[coord.Coordinate]Asset{
			coord.MustParse("app/geo@2"):    {URL: "https://example.com/two"},
			coord.MustParse("app/geo@1"):    {URL: "https://example.com/one"},
			coord.MustParse("app/alpha@1"):  {URL: "https://example.com/alpha"},
			coord.MustParse("zeta/omega@1"): {URL: "https://example.com/omega"},
		},
	}
	var names []string
	for _, name := range manifest.Coordinates() {
		names = append(names, name.String())
	}
	want := []string{"app/alpha@1", "app/geo@1", "app/geo@2", "zeta/omega@1"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("Coordinates returned %v, want %v", names, want)
	}

	clone := manifest.Clone()
	clone.Assets[coord.MustParse("app/new@1")] = Asset{URL: "https://example.com/new"}
	delete(clone.Assets, coord.MustParse("app/geo@1"))
	if len(manifest.Assets) != 4 {
		t.Fatalf("changing a clone changed the original: %d assets", len(manifest.Assets))
	}
	if _, found := manifest.Assets[coord.MustParse("app/new@1")]; found {
		t.Fatal("an asset added to a clone reached the original")
	}
}

// TestWriteRoundTripsOneFile covers the single-file writer, which init and the
// offline form of add use where there is no lock to keep in step.
func TestWriteRoundTripsOneFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dac.json")
	manifest := Manifest{
		SchemaVersion: ManifestVersion,
		Assets: map[coord.Coordinate]Asset{
			coord.MustParse("app/geo@1"): {URL: "https://example.com/geo.bin"},
		},
	}
	if err := Write(path, manifest); err != nil {
		t.Fatal(err)
	}
	readBack, err := ReadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if readBack.Assets[coord.MustParse("app/geo@1")].URL != "https://example.com/geo.bin" {
		t.Fatalf("the manifest did not survive the round trip: %#v", readBack)
	}
}

// Two versions of two different assets naming one object is not a collision.
// The cache stores it once, and a namespace exists so that two projects can
// vendor the same file without either one owning it.
func TestLockAllowsTwoAssetsThatShareAnObject(t *testing.T) {
	value := digest.Bytes([]byte("bytes"))
	manifest := Manifest{
		SchemaVersion: ManifestVersion,
		Assets: map[coord.Coordinate]Asset{
			coord.MustParse("backend/geo@1"):  {URL: "https://example.com/geo"},
			coord.MustParse("frontend/geo@1"): {URL: "https://example.com/geo"},
		},
	}
	if _, err := NewLock(manifest, map[coord.Coordinate]LockAsset{
		coord.MustParse("backend/geo@1"):  {URL: "https://example.com/geo", Digest: value, Size: 5},
		coord.MustParse("frontend/geo@1"): {URL: "https://example.com/geo", Digest: value, Size: 5},
	}); err != nil {
		t.Fatal(err)
	}
}

// A file name decides nothing, so it belongs to no comparison that asks whether
// a lock still describes a manifest. Every lock written before the field
// existed carries none, and comparing it would report every asset of every
// existing project as stale.
func TestAFileNameNeverMakesALockStale(t *testing.T) {
	value := digest.Bytes([]byte("geo"))
	name := coord.MustParse("app/geo@1")
	manifest := Manifest{
		SchemaVersion: ManifestVersion,
		Assets:        map[coord.Coordinate]Asset{name: {URL: "https://example.com/geo.bin"}},
	}
	source := manifest.Assets[name]
	for _, recorded := range []string{"", "geo.bin", "something-else.bin"} {
		locked := LockAsset{URL: "https://example.com/geo.bin", Digest: value, Size: 3, Filename: recorded}
		if !Agrees(source, locked, true) {
			t.Fatalf("a lock naming the file %q was reported as stale", recorded)
		}
		lock, err := NewLock(manifest, map[coord.Coordinate]LockAsset{name: locked})
		if err != nil {
			t.Fatalf("%q: %v", recorded, err)
		}
		if err := CheckLock(manifest, lock); err != nil {
			t.Fatalf("a lock naming the file %q did not describe its manifest: %v", recorded, err)
		}
	}
}

// The field is optional on the way in as well as out: a lock file DAC wrote
// before it existed has to keep reading.
func TestLockWithNoFileNameSurvivesARoundTrip(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "dac-lock.json")
	value := digest.Bytes([]byte("geo"))
	name := coord.MustParse("app/geo@1")
	if err := os.WriteFile(path, []byte(`{
  "lockVersion": 2,
  "manifestDigest": "`+digest.Bytes([]byte("manifest"))+`",
  "assets": {
    "app/geo@1": {"url": "https://example.com/geo.bin", "digest": "`+value+`", "size": 3}
  }
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	lock, err := ReadLock(path)
	if err != nil {
		t.Fatalf("a lock with no file name was rejected: %v", err)
	}
	if lock.Assets[name].Filename != "" {
		t.Fatalf("the entry gained the name %q on read", lock.Assets[name].Filename)
	}
	// Writing it back must not invent the field either, so a project that never
	// resolves anything keeps a stable lock file.
	if err := Write(path, lock); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "filename") {
		t.Fatalf("writing an entry with no name emitted the field: %s", data)
	}
}
