// Package config reads DAC's machine and site configuration.
// A manifest says what a project uses.
// XDG paths let site and user configuration merge without shared path knowledge.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/tomdoesdev/dac/internal/bytesize"
)

// FileName is the config file every search location looks for.
const FileName = "config.toml"

// DirName is the directory that holds DAC's config in each XDG base directory.
const DirName = "dac"

// SchemaVersion is the config layout this DAC understands.
const SchemaVersion = 2

// NoSizeLimit is the only max-size value that removes the download bound.
const NoSizeLimit = "none"

// Defaults are the values DAC uses when no config file names one.
const (
	DefaultTimeout       = "5m"
	DefaultRetries       = 2
	DefaultConcurrency   = 4
	DefaultDownloadParts = 4
	DefaultMaxSize       = "2GiB"
	DefaultMaxAge        = "30d"
	// A cache nobody bounded is unbounded.
	DefaultCacheMaxSize = NoSizeLimit
	// Hosts turn over far more slowly than the objects they served, so trust is
	// collected on its own much longer clock.
	DefaultTrustMaxAge = "180d"
)

// DefaultSource is the source name reported for a value no file supplied.
const DefaultSource = "default"

// Config holds the resolved settings for one DAC run.
type Config struct {
	Timeout       time.Duration
	Retries       int
	Concurrency   int
	DownloadParts int
	// MaxSize bounds a download whose size DAC does not already know.
	MaxSize  int64
	Progress bool
	CacheDir string
	MaxAge   time.Duration
	// CacheMaxSize bounds what the cache holds after a collection.
	CacheMaxSize int64
	// TrustFile names the trusted-hosts file, or is empty to use the XDG data location.
	TrustFile string
	// TrustMaxAge is how long a trusted host survives without a download reaching it.
	TrustMaxAge time.Duration
	// Files are the config files that were read, most important first.
	Files []string
	// Sources maps each setting to the file that supplied it, or DefaultSource.
	Sources map[string]string
}

// Load reads the config files for one DAC run.
// explicit names a file that must exist, which is what --config and DAC_CONFIG pass.
func Load(explicit string) (*Config, error) {
	paths, err := searchPaths(explicit)
	if err != nil {
		return nil, err
	}
	var files []*file
	for _, path := range paths {
		parsed, err := readFile(path, explicit != "")
		if err != nil {
			return nil, err
		}
		if parsed != nil {
			files = append(files, parsed)
		}
	}
	return merge(files)
}

// searchPaths returns the files to read, most important first.
// Search returns all XDG candidates so merge can select each setting by priority.
func searchPaths(explicit string) ([]string, error) {
	if explicit != "" {
		return []string{explicit}, nil
	}
	var paths []string
	if home := configHome(); home != "" {
		paths = append(paths, filepath.Join(home, DirName, FileName))
	}
	for _, directory := range configDirs() {
		paths = append(paths, filepath.Join(directory, DirName, FileName))
	}
	return paths, nil
}

// configHome returns $XDG_CONFIG_HOME, or ~/.config when it is unset.
// Relative XDG paths are ignored so builds cannot load config from their work directory.
func configHome() string {
	if value := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(value) {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return ""
	}
	return filepath.Join(home, ".config")
}

// configDirs returns $XDG_CONFIG_DIRS, or /etc/xdg when it is unset.
func configDirs() []string {
	value := os.Getenv("XDG_CONFIG_DIRS")
	if value == "" {
		return []string{"/etc/xdg"}
	}
	var directories []string
	for entry := range strings.SplitSeq(value, string(os.PathListSeparator)) {
		if filepath.IsAbs(entry) {
			directories = append(directories, entry)
		}
	}
	return directories
}

// file is one parsed config file.
type file struct {
	path string
	data fileData
}

type fileData struct {
	SchemaVersion *int         `toml:"schema-version"`
	Transfer      fileTransfer `toml:"transfer"`
	Cache         fileCache    `toml:"cache"`
	Trust         fileTrust    `toml:"trust"`
}

type fileTransfer struct {
	Timeout       *string `toml:"timeout"`
	Retries       *int    `toml:"retries"`
	Concurrency   *int    `toml:"concurrency"`
	DownloadParts *int    `toml:"download-parts"`
	MaxSize       *string `toml:"max-size"`
	Progress      *bool   `toml:"progress"`
}

type fileCache struct {
	Dir     *string `toml:"dir"`
	MaxAge  *string `toml:"max-age"`
	MaxSize *string `toml:"max-size"`
}

type fileTrust struct {
	File   *string `toml:"file"`
	MaxAge *string `toml:"max-age"`
}

// readFile parses one config file.
func readFile(path string, required bool) (*file, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		if required {
			return nil, fmt.Errorf("%s: the config file does not exist", path)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	parsed := &file{path: path}
	metadata, err := toml.DecodeFile(path, &parsed.data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) > 0 {
		names := make([]string, 0, len(undecoded))
		for _, key := range undecoded {
			names = append(names, key.String())
		}
		return nil, fmt.Errorf("%s: unknown %s: %s", path, plural(len(names), "key"), strings.Join(names, ", "))
	}
	if version := parsed.data.SchemaVersion; version != nil && *version != SchemaVersion {
		return nil, fmt.Errorf("%s: schema-version %d is not supported, DAC understands %d", path, *version, SchemaVersion)
	}
	return parsed, nil
}

// merge settles each setting from the most important file that supplies it.
// Each scalar comes from the first file that supplies it.
func merge(files []*file) (*Config, error) {
	config := &Config{Sources: map[string]string{}}
	for _, parsed := range files {
		config.Files = append(config.Files, parsed.path)
	}

	timeout := setting[string]{name: "transfer.timeout"}
	retries := setting[int]{name: "transfer.retries"}
	concurrency := setting[int]{name: "transfer.concurrency"}
	parts := setting[int]{name: "transfer.download-parts"}
	maxSize := setting[string]{name: "transfer.max-size"}
	progress := setting[bool]{name: "transfer.progress"}
	cacheDir := setting[string]{name: "cache.dir"}
	maxAge := setting[string]{name: "cache.max-age"}
	cacheMaxSize := setting[string]{name: "cache.max-size"}
	trustFile := setting[string]{name: "trust.file"}
	trustMaxAge := setting[string]{name: "trust.max-age"}

	for _, parsed := range files {
		data := parsed.data
		timeout.take(data.Transfer.Timeout, parsed.path)
		retries.take(data.Transfer.Retries, parsed.path)
		concurrency.take(data.Transfer.Concurrency, parsed.path)
		parts.take(data.Transfer.DownloadParts, parsed.path)
		maxSize.take(data.Transfer.MaxSize, parsed.path)
		progress.take(data.Transfer.Progress, parsed.path)
		cacheDir.take(data.Cache.Dir, parsed.path)
		maxAge.take(data.Cache.MaxAge, parsed.path)
		cacheMaxSize.take(data.Cache.MaxSize, parsed.path)
		trustFile.take(data.Trust.File, parsed.path)
		trustMaxAge.take(data.Trust.MaxAge, parsed.path)
	}

	var err error
	config.Retries = scalar(config, retries, DefaultRetries)
	config.Concurrency = scalar(config, concurrency, DefaultConcurrency)
	config.DownloadParts = scalar(config, parts, DefaultDownloadParts)
	config.Progress = scalar(config, progress, true)
	config.CacheDir = scalar(config, cacheDir, "")
	config.TrustFile = scalar(config, trustFile, "")
	if config.Timeout, err = converted(config, timeout, DefaultTimeout, ParseDuration); err != nil {
		return nil, err
	}
	if config.MaxSize, err = converted(config, maxSize, DefaultMaxSize, ParseSize); err != nil {
		return nil, err
	}
	if config.MaxAge, err = converted(config, maxAge, DefaultMaxAge, ParseDuration); err != nil {
		return nil, err
	}
	if config.CacheMaxSize, err = converted(config, cacheMaxSize, DefaultCacheMaxSize, ParseSize); err != nil {
		return nil, err
	}
	if config.TrustMaxAge, err = converted(config, trustMaxAge, DefaultTrustMaxAge, ParseDuration); err != nil {
		return nil, err
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	return config, nil
}

// setting records one value and the file it came from while a merge runs.
type setting[T any] struct {
	name   string
	value  *T
	source string
}

// take accepts a value only if a more important file has already supplied one, which is what makes the first file in the search path win each setting.
func (current *setting[T]) take(value *T, path string) {
	if value == nil || current.value != nil {
		return
	}
	current.value, current.source = value, path
}

// scalar settles a setting whose config form is already its final type.
func scalar[T any](config *Config, current setting[T], fallback T) T {
	config.Sources[current.name] = source(current.source)
	if current.value == nil {
		return fallback
	}
	return *current.value
}

// converted settles a setting written as text, such as a duration or a byte count.
func converted[R any](config *Config, current setting[string], fallback string, parse func(string) (R, error)) (R, error) {
	config.Sources[current.name] = source(current.source)
	text := fallback
	if current.value != nil {
		text = *current.value
	}
	value, err := parse(text)
	if err != nil {
		return value, fmt.Errorf("%s: %s is invalid: %w", source(current.source), current.name, err)
	}
	return value, nil
}

func source(path string) string {
	if path == "" {
		return DefaultSource
	}
	return path
}

// validate rejects the settings that have a meaningful range, so a bad config fails when it is read rather than in the middle of a transfer.
func (config *Config) validate() error {
	if config.Timeout <= 0 {
		return configError(config, "transfer.timeout", "must be positive")
	}
	if config.Retries < 0 {
		return configError(config, "transfer.retries", "must not be negative")
	}
	if config.Concurrency < 1 {
		return configError(config, "transfer.concurrency", "must be at least 1")
	}
	if config.DownloadParts < 1 {
		return configError(config, "transfer.download-parts", "must be at least 1")
	}
	if config.MaxAge < 0 {
		return configError(config, "cache.max-age", "must not be negative")
	}
	if config.CacheDir != "" && !filepath.IsAbs(config.CacheDir) {
		return configError(config, "cache.dir", "must be an absolute path")
	}
	if config.TrustMaxAge < 0 {
		return configError(config, "trust.max-age", "must not be negative")
	}
	if config.TrustFile != "" && !filepath.IsAbs(config.TrustFile) {
		return configError(config, "trust.file", "must be an absolute path")
	}
	return nil
}

func configError(config *Config, key, problem string) error {
	return fmt.Errorf("%s: %s %s", config.Sources[key], key, problem)
}

// Setting is one effective value and where it came from.
type Setting struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// settingKey is one setting and everything the TOML form needs to write it.
// Rendering reads this table instead of switching on key names, so a setting added here reaches both
// Settings and TOML from one edit. The shape this replaced decided quoting in a switch with no default,
// where a setting nobody added a case for was dropped from dac config show with no error at all.
type settingKey struct {
	name  string
	value string
	// section names the TOML table this key opens, and is empty for a key that continues the current one.
	section string
	// quoted writes the value as a TOML string rather than a bare literal.
	quoted bool
	// unsetNote replaces the assignment when the value is empty, for the paths DAC resolves for itself.
	unsetNote string
}

// settingKeys returns every effective value in the order the TOML form writes them.
func (config *Config) settingKeys() []settingKey {
	return []settingKey{
		{name: "transfer.timeout", value: FormatDuration(config.Timeout), quoted: true},
		{name: "transfer.retries", value: strconv.Itoa(config.Retries)},
		{name: "transfer.concurrency", value: strconv.Itoa(config.Concurrency)},
		{name: "transfer.download-parts", value: strconv.Itoa(config.DownloadParts)},
		{name: "transfer.max-size", value: sizeText(config.MaxSize), quoted: true},
		{name: "transfer.progress", value: strconv.FormatBool(config.Progress)},
		{name: "cache.dir", value: config.CacheDir, section: "cache", quoted: true,
			unsetNote: "# dir is unset, so DAC uses the XDG cache location"},
		{name: "cache.max-age", value: FormatDuration(config.MaxAge), quoted: true},
		{name: "cache.max-size", value: sizeText(config.CacheMaxSize), quoted: true},
		{name: "trust.file", value: config.TrustFile, section: "trust", quoted: true,
			unsetNote: "# file is unset, so DAC uses the XDG data location"},
		{name: "trust.max-age", value: FormatDuration(config.TrustMaxAge), quoted: true},
	}
}

// Settings returns every effective value with the file that supplied it, in the order the TOML form writes them.
func (config *Config) Settings() []Setting {
	keys := config.settingKeys()
	settings := make([]Setting, 0, len(keys))
	for _, key := range keys {
		settings = append(settings, Setting{Key: key.name, Value: key.value, Source: config.Sources[key.name]})
	}
	return settings
}

// sizeText writes a byte bound the way a config file would, where the value that removes the bound is a word rather than a zero.
func sizeText(value int64) string {
	if value == 0 {
		return NoSizeLimit
	}
	return bytesize.Format(value)
}

// FormatDuration writes a duration the way a config file would.
// Go's own form spells a month "720h0m0s", which is the number a cache policy is never written in.
func FormatDuration(value time.Duration) string {
	for _, unit := range []struct {
		suffix string
		size   time.Duration
	}{{"w", ageUnits["w"]}, {"d", ageUnits["d"]}} {
		if value >= unit.size && value%unit.size == 0 {
			return strconv.FormatInt(int64(value/unit.size), 10) + unit.suffix
		}
	}
	return value.String()
}

// TOML renders the effective config as a config file.
// The output is a file that loads back to the same settings, so the answer to "what is DAC actually using" is also the starting point for changing it.
func (config *Config) TOML() string {
	var text strings.Builder
	_, _ = fmt.Fprintf(&text, "schema-version = %d\n\n[transfer]\n", SchemaVersion)
	for _, key := range config.settingKeys() {
		if key.section != "" {
			_, _ = fmt.Fprintf(&text, "\n[%s]\n", key.section)
		}
		// Nothing configured the value, so DAC resolves it and says where from.
		if key.value == "" && key.unsetNote != "" {
			text.WriteString(key.unsetNote + "\n")
			continue
		}
		name := key.name[strings.Index(key.name, ".")+1:]
		source := config.Sources[key.name]
		if key.quoted {
			_, _ = fmt.Fprintf(&text, "%s = %q  # %s\n", name, key.value, source)
			continue
		}
		_, _ = fmt.Fprintf(&text, "%s = %s  # %s\n", name, key.value, source)
	}
	return text.String()
}

// ParseSize reads a max-size value, where NoSizeLimit reports no bound as zero.
func ParseSize(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if strings.EqualFold(trimmed, NoSizeLimit) {
		return 0, nil
	}
	size, err := bytesize.Parse(trimmed)
	if err != nil {
		return 0, err
	}
	if size <= 0 {
		return 0, fmt.Errorf("size %q must be positive, or %s for no bound", value, NoSizeLimit)
	}
	return size, nil
}

// ageUnits extend Go durations with the periods a cache lifetime is actually written in.
var ageUnits = map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour}

// ParseDuration reads a Go duration, or one written in days or weeks.
func ParseDuration(value string) (time.Duration, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, errors.New("the duration is empty")
	}
	if unit, exists := ageUnits[trimmed[len(trimmed)-1:]]; exists {
		count, err := strconv.ParseFloat(trimmed[:len(trimmed)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("duration %q is invalid", value)
		}
		if count < 0 {
			return 0, fmt.Errorf("duration %q must not be negative", value)
		}
		return time.Duration(count * float64(unit)), nil
	}
	age, err := time.ParseDuration(trimmed)
	if err != nil {
		return 0, fmt.Errorf("duration %q is invalid", value)
	}
	if age < 0 {
		return 0, fmt.Errorf("duration %q must not be negative", value)
	}
	return age, nil
}

func plural(count int, noun string) string {
	if count == 1 {
		return noun
	}
	return noun + "s"
}
