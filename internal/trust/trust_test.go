package trust

import (
	"slices"
	"testing"
	"time"
)

// TestNormalizeReducesAHostToWhatDACMatchesOn covers the matching rule itself.
// Trust is an exact comparison, so every difference this does not remove is a
// host somebody trusted that DAC will still refuse.
func TestNormalizeReducesAHostToWhatDACMatchesOn(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "already normal", input: "example.com", want: "example.com"},
		{name: "mixed case", input: "GitHub.COM", want: "github.com"},
		{name: "port", input: "example.com:8443", want: "example.com"},
		{name: "surrounding space", input: "  example.com\t", want: "example.com"},
		// A trailing dot is the same name written as a fully qualified one.
		{name: "root dot", input: "example.com.", want: "example.com"},
		{name: "loopback", input: "127.0.0.1:57231", want: "127.0.0.1"},
		{name: "bracketed IPv6 with a port", input: "[::1]:8080", want: "::1"},
		{name: "bracketed IPv6", input: "[::1]", want: "::1"},
		// SplitHostPort refuses a bare IPv6 literal, and the value has to survive that.
		{name: "bare IPv6", input: "::1", want: "::1"},
		{name: "empty", input: "", want: ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := Normalize(test.input); got != test.want {
				t.Fatalf("Normalize(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

// TestParseHostAcceptsAURLAsWellAsAHost covers what somebody can type at dac trust add.
// The value at hand is the one in the manifest, so pasting it has to work.
func TestParseHostAcceptsAURLAsWellAsAHost(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "bare host", input: "example.com", want: "example.com"},
		{name: "full URL", input: "HTTPS://GitHub.COM:443/owner/repo", want: "github.com"},
		{name: "URL with credentials", input: "https://user@example.com/asset", want: "example.com"},
		{name: "host and port", input: "127.0.0.1:8080", want: "127.0.0.1"},
		{name: "empty", input: "  ", wantErr: true},
		{name: "URL with no host", input: "https:///asset", wantErr: true},
		// A path is the commonest mistyped host, and it must not become one.
		{name: "host with a path", input: "example.com/asset", wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseHost(test.input)
			if test.wantErr {
				if err == nil {
					t.Fatalf("ParseHost(%q) = %q, want an error", test.input, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("ParseHost(%q) = %q, %v, want %q", test.input, got, err, test.want)
			}
		})
	}
}

func TestAddTrustsAHostOnceAndReportsWhatItAdded(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	list, added := Empty().Add([]string{"Example.com", "example.com:443", "other.example"}, now)
	if !slices.Equal(added, []string{"example.com", "other.example"}) {
		t.Fatalf("added = %v, want the two distinct hosts", added)
	}
	if len(list.Hosts) != 2 {
		t.Fatalf("hosts = %v, want two entries", list.Hosts)
	}
	if list.Hosts["example.com"].AddedAt != now {
		t.Fatalf("addedAt = %v, want %v", list.Hosts["example.com"].AddedAt, now)
	}
	// Adding again neither reports an addition nor resets the time it was trusted.
	later := now.Add(time.Hour)
	again, addedAgain := list.Add([]string{"example.com"}, later)
	if len(addedAgain) != 0 || again.Hosts["example.com"].AddedAt != now {
		t.Fatalf("re-add reported %v and set %v, want no addition and %v",
			addedAgain, again.Hosts["example.com"].AddedAt, now)
	}
}

// TestOperationsDoNotChangeTheListTheyWereGiven matters because the CLI holds one
// loaded list while it changes another, and a shared map would tie the two together.
func TestOperationsDoNotChangeTheListTheyWereGiven(t *testing.T) {
	now := time.Now()
	original, _ := Empty().Add([]string{"example.com"}, now)
	if _, removed := original.Remove([]string{"example.com"}); len(removed) != 1 {
		t.Fatalf("removed = %v, want the host", removed)
	}
	if !original.Trusted("example.com") {
		t.Fatal("Remove changed the list it was given")
	}
	original.Touch([]string{"example.com"}, now.Add(time.Hour))
	if !original.Hosts["example.com"].LastUsed.IsZero() {
		t.Fatal("Touch changed the list it was given")
	}
}

func TestRemoveReportsOnlyTheHostsItHeld(t *testing.T) {
	list, _ := Empty().Add([]string{"example.com"}, time.Now())
	updated, removed := list.Remove([]string{"example.com", "absent.example"})
	if !slices.Equal(removed, []string{"example.com"}) {
		t.Fatalf("removed = %v, want only example.com", removed)
	}
	if len(updated.Hosts) != 0 {
		t.Fatalf("hosts = %v, want none", updated.Hosts)
	}
}

// TestTouchIgnoresAHostTheListDoesNotName keeps a run that was already in flight
// from putting back a host somebody removed while it ran.
func TestTouchIgnoresAHostTheListDoesNotName(t *testing.T) {
	now := time.Now()
	list, _ := Empty().Add([]string{"example.com"}, now)
	updated := list.Touch([]string{"example.com", "removed.example"}, now.Add(time.Hour))
	if _, exists := updated.Hosts["removed.example"]; exists {
		t.Fatal("Touch created an entry for a host the list did not name")
	}
	if !updated.Hosts["example.com"].LastUsed.Equal(now.Add(time.Hour)) {
		t.Fatalf("lastUsed = %v, want the touch time", updated.Hosts["example.com"].LastUsed)
	}
}

func TestCollectDropsHostsLastUsedBeforeTheCutoff(t *testing.T) {
	cutoff := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	list := List{SchemaVersion: SchemaVersion, Hosts: map[string]Entry{
		"stale.example":  {AddedAt: cutoff.Add(-90 * 24 * time.Hour), LastUsed: cutoff.Add(-time.Second)},
		"fresh.example":  {AddedAt: cutoff.Add(-90 * 24 * time.Hour), LastUsed: cutoff},
		"unused.example": {AddedAt: cutoff.Add(-time.Second)},
		// A hand-written entry states trust without stating when, so nothing about it is stale.
		"written.example": {},
	}}
	updated, collected := list.Collect(cutoff)
	if !slices.Equal(collected, []string{"stale.example", "unused.example"}) {
		t.Fatalf("collected = %v, want the stale and the never-used host", collected)
	}
	if !slices.Equal(updated.Sorted(), []string{"fresh.example", "written.example"}) {
		t.Fatalf("kept = %v, want the fresh and the hand-written host", updated.Sorted())
	}
}

// TestNormalizeRefusesAFileNamingOneHostTwice covers the hand-edited file.
// The two entries carry different times and DAC cannot choose between them, so
// it says so rather than keeping whichever the map happened to yield first.
func TestNormalizeRefusesAFileNamingOneHostTwice(t *testing.T) {
	list := List{SchemaVersion: SchemaVersion, Hosts: map[string]Entry{
		"GitHub.com": {AddedAt: time.Now()},
		"github.com": {AddedAt: time.Now()},
	}}
	if err := list.Normalize(); err == nil {
		t.Fatal("Normalize accepted a list naming one host twice")
	}
}

func TestNormalizeRewritesAHostIntoTheFormDACMatchesOn(t *testing.T) {
	added := time.Now()
	list := List{SchemaVersion: SchemaVersion, Hosts: map[string]Entry{"GitHub.com:443": {AddedAt: added}}}
	if err := list.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if err := list.Validate(); err != nil {
		t.Fatalf("Validate after Normalize: %v", err)
	}
	if !list.Trusted("github.com") || list.Hosts["github.com"].AddedAt != added {
		t.Fatalf("hosts = %v, want github.com carrying the original time", list.Hosts)
	}
}

func TestValidateRefusesAFileDACDoesNotUnderstand(t *testing.T) {
	cases := []struct {
		name string
		list List
	}{
		{name: "future schema", list: List{SchemaVersion: SchemaVersion + 1, Hosts: map[string]Entry{}}},
		{name: "no hosts object", list: List{SchemaVersion: SchemaVersion}},
		{name: "empty host", list: List{SchemaVersion: SchemaVersion, Hosts: map[string]Entry{"": {}}}},
		{name: "host that is not normal", list: List{SchemaVersion: SchemaVersion, Hosts: map[string]Entry{"Example.com": {}}}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := test.list.Validate(); err == nil {
				t.Fatal("Validate accepted a list DAC cannot use")
			}
		})
	}
}

func TestTrustedComparesTheNormalizedHost(t *testing.T) {
	list, _ := Empty().Add([]string{"example.com"}, time.Now())
	for _, host := range []string{"example.com", "Example.COM", "example.com:8443", "example.com."} {
		if !list.Trusted(host) {
			t.Fatalf("Trusted(%q) = false, want true", host)
		}
	}
	if list.Trusted("other.example.com") {
		t.Fatal("Trusted matched a subdomain, and matching is exact")
	}
}
