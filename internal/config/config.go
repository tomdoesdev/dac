// Package config reads DAC's machine and site configuration.
//
// A manifest says what a project uses. This says how the machine running DAC
// fetches it: how long to wait, how hard to retry, which credential helper
// answers for which host, and where requests actually go. Those are properties
// of a deployment rather than of a run, and they used to be flags because there
// was nowhere else to put them -- which meant every command that could honour
// one had to carry it, and a site that proxied its downloads had to teach every
// script the same six options.
//
// The file lives where the XDG base directory specification says it does, so a
// site can install one under /etc/xdg and a person can override any part of it
// under ~/.config without either having to know about the other.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/tomdoesdev/dac/internal/bytesize"
	"github.com/tomdoesdev/dac/internal/rewrite"
)

// FileName is the config file every search location looks for.
const FileName = "config.toml"

// DirName is the directory that holds DAC's config in each XDG base directory.
const DirName = "dac"

// SchemaVersion is the config layout this DAC understands. The key is optional
// -- a hand-written file should not have to carry bookkeeping to work -- but a
// file that states a version must state one DAC knows, so a config written for
// a later DAC fails here rather than by having its unknown keys rejected one at
// a time.
const SchemaVersion = 1

// NoSizeLimit is the only max-size value that removes the download bound.
//
// An empty value and a zero used to do it too, and neither said so. Both are
// what a shell produces from a variable nobody set, so the guard against a
// runaway stream could end up disabled by a deployment that thought it was
// leaving the default in place. Switching it off is a decision, so it needs a
// word.
const NoSizeLimit = "none"

// Defaults are the values DAC uses when no config file names one. The ones
// spelled as text are the same text a config file would carry, so the default
// and a configured value go through one parser and cannot disagree.
const (
	DefaultTimeout       = "5m"
	DefaultRetries       = 2
	DefaultConcurrency   = 4
	DefaultDownloadParts = 4
	DefaultMaxSize       = "2GiB"
	DefaultMaxAge        = "30d"
)

// DefaultSource is the source name reported for a value no file supplied.
const DefaultSource = "default"

// Config holds the resolved settings for one DAC run.
type Config struct {
	Timeout       time.Duration
	Retries       int
	Concurrency   int
	DownloadParts int
	// MaxSize bounds a download whose size DAC does not already know. Zero
	// means no bound.
	MaxSize  int64
	Progress bool
	CacheDir string
	MaxAge   time.Duration
	// Credentials holds helper specifications in the form credential.New
	// takes: "<command>" for every host, "<host>=<command>" for one.
	Credentials []string
	// Rewrite holds URL rewrite and host policy rules, or nil when no file
	// supplied any.
	Rewrite *rewrite.Config

	// Files are the config files that were read, most important first.
	Files []string
	// Sources maps each setting to the file that supplied it, or DefaultSource.
	Sources map[string]string
}

// Load reads the config files for one DAC run.
//
// explicit names a file that must exist, which is what --config and DAC_CONFIG
// pass. When it is empty the XDG search path is used instead and every file in
// it is optional: a machine with no config is a machine that wants the
// defaults, not a machine that is misconfigured.
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
//
// The XDG specification orders base directories by importance and asks that
// configuration from several of them be merged rather than have the first found
// win outright, so this returns every candidate and merge below settles each
// setting separately.
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
//
// A relative XDG value is ignored rather than resolved against the working
// directory, which is what the specification asks for and what keeps a stray
// variable from making DAC read a file out of whatever directory a build
// happened to run in.
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
	for _, entry := range strings.Split(value, string(os.PathListSeparator)) {
		if filepath.IsAbs(entry) {
			directories = append(directories, entry)
		}
	}
	return directories
}

// file is one parsed config file. Every scalar is a pointer so that a file
// which does not mention a setting can be told apart from one that sets it to
// the zero value, which is what lets a less important file supply it instead.
type file struct {
	path string
	data fileData
}

type fileData struct {
	SchemaVersion *int              `toml:"schema-version"`
	Transfer      fileTransfer      `toml:"transfer"`
	Cache         fileCache         `toml:"cache"`
	Credentials   map[string]string `toml:"credentials"`
	Rewrite       []fileRewrite     `toml:"rewrite"`
	Hosts         fileHosts         `toml:"hosts"`
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
	Dir    *string `toml:"dir"`
	MaxAge *string `toml:"max-age"`
}

type fileRewrite struct {
	Pattern     string `toml:"pattern"`
	Replacement string `toml:"replacement"`
}

type fileHosts struct {
	Allow             []string `toml:"allow"`
	Block             []string `toml:"block"`
	AllowInsecureHTTP *bool    `toml:"allow-insecure-http"`
}

// readFile parses one config file. A missing file reports no config unless it
// was named explicitly: a search path that finds nothing means a machine that
// wants the defaults, while --config naming a file that is not there is a
// deployment that thinks it configured something.
func readFile(path string, required bool) (*file, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		if required {
			return nil, fmt.Errorf("%s: the config file does not exist", path)
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := checkPermissions(path, info); err != nil {
		return nil, err
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

// checkPermissions refuses a config file that somebody else can write.
//
// The credentials table names programs DAC runs, so a config file is a list of
// commands to execute and a writable one is a way to choose them. This is the
// posture ssh takes with its own config for the same reason. A readable file is
// fine: nothing secret belongs in here, only the name of the helper that knows
// the secret.
func checkPermissions(path string, info os.FileInfo) error {
	if mode := info.Mode().Perm(); mode&0o022 != 0 {
		return fmt.Errorf("%s: the config file is writable by group or other (mode %04o), and it names programs DAC runs", path, mode)
	}
	return nil
}

// merge settles each setting from the most important file that supplies it.
//
// Tables merge by key and arrays replace whole. A credentials table is a map
// from host to helper, so a person adding one host to what their site
// configured is adding an entry rather than restating the site's; a rewrite or
// host policy list is one policy, and half-overriding it would produce a policy
// nobody wrote.
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

	credentials := map[string]string{}
	credentialSource := ""
	var hosts fileHosts
	var rules []fileRewrite
	rewriteSource, hostsSource := "", ""

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

		for host, command := range data.Credentials {
			if _, exists := credentials[host]; exists {
				continue
			}
			credentials[host] = command
			if credentialSource == "" {
				credentialSource = parsed.path
			}
		}
		if rewriteSource == "" && data.Rewrite != nil {
			rules, rewriteSource = data.Rewrite, parsed.path
		}
		if hostsSource == "" && (data.Hosts.Allow != nil || data.Hosts.Block != nil || data.Hosts.AllowInsecureHTTP != nil) {
			hosts, hostsSource = data.Hosts, parsed.path
		}
	}

	var err error
	config.Retries = scalar(config, retries, DefaultRetries)
	config.Concurrency = scalar(config, concurrency, DefaultConcurrency)
	config.DownloadParts = scalar(config, parts, DefaultDownloadParts)
	config.Progress = scalar(config, progress, true)
	config.CacheDir = scalar(config, cacheDir, "")
	if config.Timeout, err = converted(config, timeout, DefaultTimeout, ParseDuration); err != nil {
		return nil, err
	}
	if config.MaxSize, err = converted(config, maxSize, DefaultMaxSize, ParseSize); err != nil {
		return nil, err
	}
	if config.MaxAge, err = converted(config, maxAge, DefaultMaxAge, ParseDuration); err != nil {
		return nil, err
	}
	if err := config.validate(); err != nil {
		return nil, err
	}

	config.Credentials = credentialList(credentials)
	config.Sources["credentials"] = source(credentialSource)
	config.Sources["rewrite"] = source(rewriteSource)
	config.Sources["hosts"] = source(hostsSource)
	if config.Rewrite, err = buildRewrite(rules, hosts, rewriteSource, hostsSource); err != nil {
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

// take accepts a value only if a more important file has already supplied one,
// which is what makes the first file in the search path win each setting.
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

// converted settles a setting written as text, such as a duration or a byte
// count. The default goes through the same parser as a configured value, so the
// two cannot drift apart.
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

// validate rejects the settings that have a meaningful range, so a bad config
// fails when it is read rather than in the middle of a transfer.
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
	return nil
}

func configError(config *Config, key, problem string) error {
	return fmt.Errorf("%s: %s %s", config.Sources[key], key, problem)
}

// credentialList converts the credentials table into the specifications
// credential.New takes. The "default" key is the helper for every host, which
// is the table's way of spelling a specification with no host in front of it.
func credentialList(credentials map[string]string) []string {
	if len(credentials) == 0 {
		return nil
	}
	specifications := make([]string, 0, len(credentials))
	for host, command := range credentials {
		if host == "default" {
			specifications = append(specifications, command)
			continue
		}
		specifications = append(specifications, host+"="+command)
	}
	// A map has no order and credential.New reads the list in order, so sort
	// it: a config that produced a different resolver on each run would be a
	// very hard thing to be sure about.
	slices.Sort(specifications)
	return specifications
}

// buildRewrite turns the rewrite and hosts sections into a rewrite config. It
// reports nil when neither section names a rule, which leaves every URL alone.
func buildRewrite(rules []fileRewrite, hosts fileHosts, rewriteSource, hostsSource string) (*rewrite.Config, error) {
	if len(rules) == 0 && len(hosts.Allow) == 0 && len(hosts.Block) == 0 && hosts.AllowInsecureHTTP == nil {
		return nil, nil
	}
	options := rewrite.Options{Allow: hosts.Allow, Block: hosts.Block}
	if hosts.AllowInsecureHTTP != nil {
		options.AllowInsecureHTTP = *hosts.AllowInsecureHTTP
	}
	for _, rule := range rules {
		options.Rewrites = append(options.Rewrites, rewrite.Rule{Pattern: rule.Pattern, Replacement: rule.Replacement})
	}
	config, err := rewrite.Build(options)
	if err != nil {
		path := rewriteSource
		if path == "" {
			path = hostsSource
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return config, nil
}

// Setting is one effective value and where it came from.
type Setting struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// Settings returns every effective value with the file that supplied it, in the
// order the TOML form writes them.
func (config *Config) Settings() []Setting {
	keys := []struct {
		name  string
		value string
	}{
		{"transfer.timeout", FormatDuration(config.Timeout)},
		{"transfer.retries", strconv.Itoa(config.Retries)},
		{"transfer.concurrency", strconv.Itoa(config.Concurrency)},
		{"transfer.download-parts", strconv.Itoa(config.DownloadParts)},
		{"transfer.max-size", config.sizeText()},
		{"transfer.progress", strconv.FormatBool(config.Progress)},
		{"cache.dir", config.CacheDir},
		{"cache.max-age", FormatDuration(config.MaxAge)},
	}
	settings := make([]Setting, 0, len(keys)+1)
	for _, key := range keys {
		settings = append(settings, Setting{Key: key.name, Value: key.value, Source: config.Sources[key.name]})
	}
	settings = append(settings, Setting{
		Key:    "credentials",
		Value:  strings.Join(config.Credentials, " "),
		Source: config.Sources["credentials"],
	})
	return settings
}

func (config *Config) sizeText() string {
	if config.MaxSize == 0 {
		return NoSizeLimit
	}
	return bytesize.Format(config.MaxSize)
}

// FormatDuration writes a duration the way a config file would.
//
// Go's own form spells a month "720h0m0s", which is the number a cache policy
// is never written in. Whole days and weeks get their own units back, and
// anything else falls through to the Go form, which ParseDuration reads either
// way -- so what Settings prints always loads again.
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
//
// The output is a file that loads back to the same settings, so the answer to
// "what is DAC actually using" is also the starting point for changing it. Each
// value carries the file it came from as a comment, because a merge across the
// XDG search path is exactly the situation where that is not obvious.
func (config *Config) TOML() string {
	var text strings.Builder
	_, _ = fmt.Fprintf(&text, "schema-version = %d\n\n[transfer]\n", SchemaVersion)
	for _, setting := range config.Settings() {
		switch setting.Key {
		case "cache.dir":
			text.WriteString("\n[cache]\n")
			if config.CacheDir == "" {
				// Nothing configured it, so DAC resolves it from the XDG cache
				// location. Writing an empty string back would be a value.
				_, _ = fmt.Fprintf(&text, "# dir is unset, so DAC uses the XDG cache location\n")
				continue
			}
			_, _ = fmt.Fprintf(&text, "dir = %q  # %s\n", config.CacheDir, setting.Source)
			continue
		case "credentials":
			text.WriteString("\n[credentials]\n")
			for _, specification := range config.Credentials {
				host, command, found := strings.Cut(specification, "=")
				if !found {
					host, command = "default", specification
				}
				_, _ = fmt.Fprintf(&text, "%q = %q  # %s\n", host, command, setting.Source)
			}
			continue
		}
		key := setting.Key[strings.Index(setting.Key, ".")+1:]
		switch setting.Key {
		case "transfer.retries", "transfer.concurrency", "transfer.download-parts", "transfer.progress":
			_, _ = fmt.Fprintf(&text, "%s = %s  # %s\n", key, setting.Value, setting.Source)
		case "transfer.timeout", "transfer.max-size", "cache.max-age":
			_, _ = fmt.Fprintf(&text, "%s = %q  # %s\n", key, setting.Value, setting.Source)
		}
	}
	if config.Rewrite != nil {
		_, _ = fmt.Fprintf(&text, "\n# Rewrite and host policy rules come from %s and %s.\n",
			config.Sources["rewrite"], config.Sources["hosts"])
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

// ageUnits extend Go durations with the periods a cache lifetime is actually
// written in. Nobody sets a cache policy in hours.
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
