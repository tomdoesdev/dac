package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tomdoesdev/dac/internal/config"
)

// isolate points the XDG search path at temporary directories, so a test reads
// the files it wrote rather than whatever the machine running it has installed.
// It returns the user config directory and the system one, most important
// first.
func isolate(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	userHome := filepath.Join(root, "user")
	systemDir := filepath.Join(root, "system")
	for _, directory := range []string{userHome, systemDir} {
		if err := os.MkdirAll(filepath.Join(directory, "dac"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", userHome)
	t.Setenv("XDG_CONFIG_DIRS", systemDir)
	t.Setenv("HOME", root)
	return userHome, systemDir
}

func write(t *testing.T, directory, text string) string {
	t.Helper()
	path := filepath.Join(directory, "dac", config.FileName)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func load(t *testing.T, explicit string) *config.Config {
	t.Helper()
	loaded, err := config.Load(explicit)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return loaded
}

func loadError(t *testing.T, explicit string) error {
	t.Helper()
	loaded, err := config.Load(explicit)
	if err == nil {
		t.Fatalf("load succeeded, want an error: %+v", loaded)
	}
	return err
}

func TestLoadWithoutAnyFileUsesDefaults(t *testing.T) {
	isolate(t)
	loaded := load(t, "")
	if loaded.Timeout != 5*time.Minute {
		t.Errorf("timeout = %v, want 5m", loaded.Timeout)
	}
	if loaded.Retries != 2 {
		t.Errorf("retries = %d, want 2", loaded.Retries)
	}
	if loaded.Concurrency != 4 {
		t.Errorf("concurrency = %d, want 4", loaded.Concurrency)
	}
	if loaded.DownloadParts != 4 {
		t.Errorf("download parts = %d, want 4", loaded.DownloadParts)
	}
	if loaded.MaxSize != 2<<30 {
		t.Errorf("max size = %d, want 2GiB", loaded.MaxSize)
	}
	if !loaded.Progress {
		t.Error("progress is disabled by default")
	}
	if loaded.MaxAge != 30*24*time.Hour {
		t.Errorf("max age = %v, want 30d", loaded.MaxAge)
	}
	if len(loaded.Files) != 0 {
		t.Errorf("files = %v, want none", loaded.Files)
	}
	if got := loaded.Sources["transfer.timeout"]; got != config.DefaultSource {
		t.Errorf("timeout source = %q, want %q", got, config.DefaultSource)
	}
}

func TestLoadReadsTheUserConfig(t *testing.T) {
	userHome, _ := isolate(t)
	path := write(t, userHome, `
[transfer]
timeout = "90s"
retries = 5
max-size = "8GiB"
progress = false

[cache]
dir = "/var/cache/dac"
max-age = "2w"
`)
	loaded := load(t, "")
	if loaded.Timeout != 90*time.Second {
		t.Errorf("timeout = %v, want 90s", loaded.Timeout)
	}
	if loaded.Retries != 5 {
		t.Errorf("retries = %d, want 5", loaded.Retries)
	}
	if loaded.MaxSize != 8<<30 {
		t.Errorf("max size = %d, want 8GiB", loaded.MaxSize)
	}
	if loaded.Progress {
		t.Error("progress stayed enabled")
	}
	if loaded.CacheDir != "/var/cache/dac" {
		t.Errorf("cache dir = %q", loaded.CacheDir)
	}
	if loaded.MaxAge != 14*24*time.Hour {
		t.Errorf("max age = %v, want 2w", loaded.MaxAge)
	}
	if loaded.Concurrency != 4 {
		t.Errorf("concurrency = %d, want the default 4", loaded.Concurrency)
	}
	if got := loaded.Sources["transfer.timeout"]; got != path {
		t.Errorf("timeout source = %q, want %q", got, path)
	}
	if got := loaded.Sources["transfer.concurrency"]; got != config.DefaultSource {
		t.Errorf("concurrency source = %q, want %q", got, config.DefaultSource)
	}
}

// A site installs one file and a person overrides the part of it they disagree
// with. That only works if the two merge setting by setting: a user file that
// mentions one key must not discard everything else the site configured.
func TestLoadMergesUserOverSystemPerKey(t *testing.T) {
	userHome, systemDir := isolate(t)
	systemPath := write(t, systemDir, `
[transfer]
timeout = "10m"
retries = 9
concurrency = 8
`)
	userPath := write(t, userHome, `
[transfer]
retries = 1
`)
	loaded := load(t, "")
	if loaded.Retries != 1 {
		t.Errorf("retries = %d, want the user value 1", loaded.Retries)
	}
	if loaded.Timeout != 10*time.Minute {
		t.Errorf("timeout = %v, want the system value 10m", loaded.Timeout)
	}
	if loaded.Concurrency != 8 {
		t.Errorf("concurrency = %d, want the system value 8", loaded.Concurrency)
	}
	if got := loaded.Sources["transfer.retries"]; got != userPath {
		t.Errorf("retries source = %q, want %q", got, userPath)
	}
	if got := loaded.Sources["transfer.timeout"]; got != systemPath {
		t.Errorf("timeout source = %q, want %q", got, systemPath)
	}
	if len(loaded.Files) != 2 || loaded.Files[0] != userPath || loaded.Files[1] != systemPath {
		t.Errorf("files = %v, want the user file first", loaded.Files)
	}
}

// A zero is a value, not an absence. A user file that sets retries to 0 has to
// beat a system file that sets it to 9, which is the whole reason the parsed
// form keeps pointers.
func TestLoadTreatsAZeroAsASetting(t *testing.T) {
	userHome, systemDir := isolate(t)
	write(t, systemDir, "[transfer]\nretries = 9\n")
	write(t, userHome, "[transfer]\nretries = 0\n")
	if loaded := load(t, ""); loaded.Retries != 0 {
		t.Errorf("retries = %d, want 0", loaded.Retries)
	}
}

func TestLoadExplicitPathMustExist(t *testing.T) {
	isolate(t)
	err := loadError(t, filepath.Join(t.TempDir(), "absent.toml"))
	if !strings.Contains(err.Error(), "absent.toml") {
		t.Errorf("error does not name the file: %v", err)
	}
}

func TestLoadExplicitPathIgnoresTheSearchPath(t *testing.T) {
	userHome, _ := isolate(t)
	write(t, userHome, "[transfer]\nretries = 9\n")
	explicit := filepath.Join(t.TempDir(), "ci.toml")
	if err := os.WriteFile(explicit, []byte("[transfer]\nretries = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded := load(t, explicit)
	if loaded.Retries != 1 {
		t.Errorf("retries = %d, want 1", loaded.Retries)
	}
	if len(loaded.Files) != 1 || loaded.Files[0] != explicit {
		t.Errorf("files = %v, want only %q", loaded.Files, explicit)
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	userHome, _ := isolate(t)
	write(t, userHome, "[transfer]\ntimeuot = \"5m\"\n")
	err := loadError(t, "")
	if !strings.Contains(err.Error(), "timeuot") {
		t.Errorf("error does not name the key: %v", err)
	}
}

func TestLoadRejectsAnUnknownSchemaVersion(t *testing.T) {
	userHome, _ := isolate(t)
	write(t, userHome, "schema-version = 99\n")
	err := loadError(t, "")
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("error does not name the version: %v", err)
	}
}

func TestLoadAcceptsTheCurrentSchemaVersion(t *testing.T) {
	userHome, _ := isolate(t)
	write(t, userHome, "schema-version = 2\n[transfer]\nretries = 3\n")
	if loaded := load(t, ""); loaded.Retries != 3 {
		t.Errorf("retries = %d, want 3", loaded.Retries)
	}
}

func TestLoadRejectsTheOldSchemaVersion(t *testing.T) {
	userHome, _ := isolate(t)
	write(t, userHome, "schema-version = 1\n")
	err := loadError(t, "")
	if !strings.Contains(err.Error(), "schema-version 1") {
		t.Errorf("error does not name the old version: %v", err)
	}
}

func TestLoadRejectsRemovedSettings(t *testing.T) {
	settings := []struct {
		text string
		key  string
	}{
		{"[credentials]\ndefault = \"helper\"\n", "credentials"},
		{"[[rewrite]]\npattern = \"x\"\nreplacement = \"y\"\n", "rewrite"},
		{"[hosts]\nblock = [\"*\"]\n", "hosts"},
	}
	for _, setting := range settings {
		userHome, _ := isolate(t)
		write(t, userHome, setting.text)
		if _, err := config.Load(""); err == nil || !strings.Contains(err.Error(), setting.key) {
			t.Fatalf("removed setting did not produce a clear error: %v", err)
		}
	}
}

func TestLoadRejectsInvalidSettings(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{"timeout", "[transfer]\ntimeout = \"0s\"\n", "transfer.timeout"},
		{"negative timeout", "[transfer]\ntimeout = \"-1m\"\n", "transfer.timeout"},
		{"retries", "[transfer]\nretries = -1\n", "transfer.retries"},
		{"concurrency", "[transfer]\nconcurrency = 0\n", "transfer.concurrency"},
		{"download parts", "[transfer]\ndownload-parts = 0\n", "transfer.download-parts"},
		{"max size", "[transfer]\nmax-size = \"0\"\n", "transfer.max-size"},
		{"unreadable max size", "[transfer]\nmax-size = \"lots\"\n", "transfer.max-size"},
		{"empty max size", "[transfer]\nmax-size = \"\"\n", "transfer.max-size"},
		{"max age", "[cache]\nmax-age = \"-1d\"\n", "cache.max-age"},
		{"relative cache dir", "[cache]\ndir = \"relative/path\"\n", "cache.dir"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			userHome, _ := isolate(t)
			write(t, userHome, test.text)
			if err := loadError(t, ""); !strings.Contains(err.Error(), test.want) {
				t.Errorf("error does not name %s: %v", test.want, err)
			}
		})
	}
}

// An empty value and a zero used to remove the download bound, and neither said
// so. Turning the guard off is a decision, so it takes a word.
func TestMaxSizeNoneRemovesTheBound(t *testing.T) {
	userHome, _ := isolate(t)
	write(t, userHome, "[transfer]\nmax-size = \"none\"\n")
	if loaded := load(t, ""); loaded.MaxSize != 0 {
		t.Errorf("max size = %d, want 0", loaded.MaxSize)
	}
}

func TestParseSize(t *testing.T) {
	cases := []struct {
		value string
		want  int64
	}{
		{"2GiB", 2 << 30},
		{"none", 0},
		{"NONE", 0},
		{" none ", 0},
		{"512", 512},
	}
	for _, test := range cases {
		got, err := config.ParseSize(test.value)
		if err != nil {
			t.Errorf("ParseSize(%q) failed: %v", test.value, err)
			continue
		}
		if got != test.want {
			t.Errorf("ParseSize(%q) = %d, want %d", test.value, got, test.want)
		}
	}
	for _, value := range []string{"", "0", "-1", "lots", "1XiB"} {
		if got, err := config.ParseSize(value); err == nil {
			t.Errorf("ParseSize(%q) = %d, want an error", value, got)
		}
	}
}

func TestParseDuration(t *testing.T) {
	cases := []struct {
		value string
		want  time.Duration
	}{
		{"30d", 30 * 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
		{"1.5d", 36 * time.Hour},
		{"90s", 90 * time.Second},
		{"5m", 5 * time.Minute},
		{" 1h ", time.Hour},
		{"0s", 0},
	}
	for _, test := range cases {
		got, err := config.ParseDuration(test.value)
		if err != nil {
			t.Errorf("ParseDuration(%q) failed: %v", test.value, err)
			continue
		}
		if got != test.want {
			t.Errorf("ParseDuration(%q) = %v, want %v", test.value, got, test.want)
		}
	}
	for _, value := range []string{"", "  ", "-1d", "-1m", "soon", "5x"} {
		if got, err := config.ParseDuration(value); err == nil {
			t.Errorf("ParseDuration(%q) = %v, want an error", value, got)
		}
	}
}

// A relative XDG value is ignored rather than resolved against the working
// directory, so a stray variable cannot make DAC read a file out of whatever
// directory a build happened to run in.
func TestLoadIgnoresARelativeConfigHome(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "relative/config")
	t.Setenv("XDG_CONFIG_DIRS", filepath.Join(root, "system"))
	t.Setenv("HOME", root)
	if err := os.MkdirAll(filepath.Join(root, ".config", "dac"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".config", "dac", config.FileName)
	if err := os.WriteFile(path, []byte("[transfer]\nretries = 7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded := load(t, "")
	if loaded.Retries != 7 {
		t.Errorf("retries = %d, want the value from ~/.config", loaded.Retries)
	}
}

// The human form of dac config show is a config file, so the answer to "what is
// DAC using" is also the starting point for changing it. That only holds if it
// loads back to the same settings.
func TestTOMLRoundTrips(t *testing.T) {
	userHome, _ := isolate(t)
	write(t, userHome, `
[transfer]
timeout = "90s"
retries = 5
concurrency = 2
download-parts = 1
max-size = "1500"
progress = false

[cache]
dir = "/var/cache/dac"
max-age = "2w"
max-size = "1048575"
`)
	first := load(t, "")

	rendered := filepath.Join(t.TempDir(), "rendered.toml")
	if err := os.WriteFile(rendered, []byte(first.TOML()), 0o600); err != nil {
		t.Fatal(err)
	}
	second := load(t, rendered)

	if first.Timeout != second.Timeout || first.Retries != second.Retries ||
		first.Concurrency != second.Concurrency || first.DownloadParts != second.DownloadParts ||
		first.MaxSize != second.MaxSize || first.Progress != second.Progress ||
		first.CacheDir != second.CacheDir || first.MaxAge != second.MaxAge ||
		first.CacheMaxSize != second.CacheMaxSize {
		t.Fatalf("settings changed across a round trip:\n%s\nfirst=%+v\nsecond=%+v", first.TOML(), first, second)
	}
}

// The defaults have to survive the same trip, because a machine with no config
// file is the most likely thing somebody runs dac config show on first.
func TestTOMLRoundTripsTheDefaults(t *testing.T) {
	isolate(t)
	first := load(t, "")
	rendered := filepath.Join(t.TempDir(), "rendered.toml")
	if err := os.WriteFile(rendered, []byte(first.TOML()), 0o600); err != nil {
		t.Fatal(err)
	}
	second := load(t, rendered)
	if first.Timeout != second.Timeout || first.MaxSize != second.MaxSize || first.MaxAge != second.MaxAge ||
		first.CacheMaxSize != second.CacheMaxSize {
		t.Fatalf("the defaults changed across a round trip:\n%s", first.TOML())
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		value time.Duration
		want  string
	}{
		{30 * 24 * time.Hour, "30d"},
		{14 * 24 * time.Hour, "2w"},
		{7 * 24 * time.Hour, "1w"},
		// A day and a half is not a whole number of days, so it keeps the Go
		// form rather than being rounded into one.
		{36 * time.Hour, "36h0m0s"},
		{5 * time.Minute, "5m0s"},
		{0, "0s"},
	}
	for _, test := range cases {
		if got := config.FormatDuration(test.value); got != test.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", test.value, got, test.want)
		}
		// Whatever it writes has to load again as the same duration.
		back, err := config.ParseDuration(config.FormatDuration(test.value))
		if err != nil {
			t.Errorf("FormatDuration(%v) does not parse: %v", test.value, err)
			continue
		}
		if back != test.value {
			t.Errorf("FormatDuration(%v) round-tripped to %v", test.value, back)
		}
	}
}

// TestCacheMaxSize covers the bound a collection leaves the cache inside.
//
// Its default is the word that removes it, because a cache nobody bounded is
// unbounded: a size that started out set would delete objects on machines whose
// operators never asked for a bound.
func TestCacheMaxSize(t *testing.T) {
	isolate(t)
	if value := load(t, "").CacheMaxSize; value != 0 {
		t.Fatalf("the default cache bound is %d, want none", value)
	}

	userHome, _ := isolate(t)
	write(t, userHome, "[cache]\nmax-size = \"20GiB\"\n")
	if value := load(t, "").CacheMaxSize; value != 20<<30 {
		t.Fatalf("cache.max-size is %d, want 20GiB", value)
	}

	userHome, _ = isolate(t)
	write(t, userHome, "[cache]\nmax-size = \"none\"\n")
	if value := load(t, "").CacheMaxSize; value != 0 {
		t.Fatalf("an explicit none is %d, want no bound", value)
	}

	// The values that mean nothing in particular are refused here for the same
	// reason transfer.max-size refuses them: both are what a shell produces
	// from a variable nobody set, and neither should be read as switching a
	// bound off by accident.
	for _, value := range []string{"0", "", "banana", "-1GiB"} {
		userHome, _ = isolate(t)
		write(t, userHome, "[cache]\nmax-size = \""+value+"\"\n")
		if _, err := config.Load(""); err == nil {
			t.Fatalf("cache.max-size %q was accepted", value)
		}
	}
}
