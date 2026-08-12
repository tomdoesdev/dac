package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/kit/fs/atomic"
)

var variableNamePattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)

// Manifest is the human-owned desired state. Maps are used only in memory;
// writes sort their keys so committed files remain deterministic.
type Manifest struct {
	Version int              `toml:"version"`
	Files   map[string]Asset `toml:"files"`
}

// Asset describes one remote artifact before its templates are rendered.
type Asset struct {
	URL       string            `toml:"url"`
	File      string            `toml:"file"`
	Pin       string            `toml:"pin,omitempty"`
	Variables map[string]string `toml:"variables"`
	Headers   map[string]string `toml:"headers"`
}

// Load strictly decodes and validates a manifest from path.
func Load(path string) (Manifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return Manifest{}, project.NewError("filesystem", err)
	}
	defer func() { _ = file.Close() }()
	var value Manifest
	decoder := toml.NewDecoder(file).DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return Manifest{}, project.NewError("configuration", fmt.Errorf("decode dac.toml: %w", err))
	}
	if value.Files == nil {
		// A freshly initialized manifest contains only its version. Treat that as
		// the natural empty desired state rather than requiring a noisy table.
		value.Files = make(map[string]Asset)
	}
	if err := Validate(value); err != nil {
		return Manifest{}, err
	}
	return value, nil
}

// Write canonically rewrites the full human-owned state atomically.
func Write(path string, value Manifest) error {
	if err := Validate(value); err != nil {
		return err
	}
	if err := atomic.WriteFile(path, encode(value), 0o644); err != nil {
		return project.NewError("filesystem", err)
	}
	return nil
}

// Create atomically initializes a manifest without replacing an existing file,
// even if two init processes race.
func Create(path string, value Manifest) error {
	if err := Validate(value); err != nil {
		return err
	}
	file, err := atomic.Create(path, 0o644)
	if err != nil {
		return project.NewError("filesystem", err)
	}
	defer func() { _ = file.Discard() }()
	if _, err := file.Write(encode(value)); err != nil {
		return project.NewError("filesystem", err)
	}
	commit, err := file.CommitNoReplace()
	if err != nil {
		if commit != nil {
			err = errors.Join(err, commit.Rollback())
		}
		if errors.Is(err, fs.ErrExist) {
			return &project.Error{Kind: "configuration", Err: errors.New("dac.toml already exists")}
		}
		return project.NewError("filesystem", err)
	}
	if err := commit.Complete(); err != nil {
		return project.NewError("filesystem", err)
	}
	return nil
}

// Validate makes every declared artifact safe to render. Resolution checks run
// afterwards because template output may introduce a collision.
func Validate(value Manifest) error {
	if value.Version != project.Version {
		return &project.Error{Kind: "configuration", Err: fmt.Errorf("unsupported dac.toml version %d", value.Version)}
	}
	for name, file := range value.Files {
		if !ValidAssetName(name) {
			return &project.Error{Kind: "configuration", Asset: name, Err: errors.New("invalid asset name")}
		}
		if !utf8.ValidString(file.URL) || !utf8.ValidString(file.File) || !utf8.ValidString(file.Pin) {
			return &project.Error{Kind: "configuration", Asset: name, Err: errors.New("url, file, and pin must be valid UTF-8")}
		}
		if strings.TrimSpace(file.URL) == "" || strings.TrimSpace(file.File) == "" {
			return &project.Error{Kind: "configuration", Asset: name, Err: errors.New("url and file are required")}
		}
		if _, err := parseTemplate("url", file.URL); err != nil {
			return &project.Error{Kind: "configuration", Asset: name, Err: err}
		}
		if _, err := parseTemplate("file", file.File); err != nil {
			return &project.Error{Kind: "configuration", Asset: name, Err: err}
		}
		if file.Pin != "" {
			if _, err := asset.NormalizeDigest(file.Pin); err != nil {
				return &project.Error{Kind: "configuration", Asset: name, Err: fmt.Errorf("invalid pin: %w", err)}
			}
		}
		for key, item := range file.Variables {
			if !variableNamePattern.MatchString(key) {
				return &project.Error{Kind: "configuration", Asset: name, Err: fmt.Errorf("invalid variable name %q", key)}
			}
			if !utf8.ValidString(item) {
				return &project.Error{Kind: "configuration", Asset: name, Err: fmt.Errorf("variable %q must be valid UTF-8", key)}
			}
		}
		if err := asset.ValidateHeaders(file.Headers); err != nil {
			return &project.Error{Kind: "configuration", Asset: name, Err: err}
		}
	}
	return nil
}

// ValidAssetName reports whether a logical asset identifier is printable and
// unambiguous in dac's line-oriented output.
func ValidAssetName(value string) bool {
	if value == "" || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if !unicode.IsPrint(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}
