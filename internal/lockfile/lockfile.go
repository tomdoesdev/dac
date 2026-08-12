package lockfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/kit/fs/atomic"
	"github.com/tomdoesdev/kit/fs/util/filename"
	"github.com/tomdoesdev/kit/strictjson"
)

// Lockfile is machine-owned accepted state. It stores only the resolution and
// bytes, never request credentials or response metadata.
type Lockfile struct {
	Version int              `json:"version"`
	Files   map[string]Asset `json:"files"`
}

// Asset records the accepted resolution and bytes for one manifest asset.
type Asset struct {
	ResolvedURL  string `json:"resolved_url"`
	ResolvedFile string `json:"resolved_file"`
	Digest       string `json:"digest"`
	Size         int64  `json:"size"`
}

// Load reads and validates a strict machine-authored lock file.
func Load(path string) (Lockfile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Lockfile{}, &project.Error{Kind: "configuration", Hint: "run `dac lock`", Err: errors.New("dac.lock does not exist")}
	}
	if err != nil {
		return Lockfile{}, project.NewError("filesystem", err)
	}
	var value Lockfile
	if err := strictjson.Unmarshal(data, &value); err != nil {
		return Lockfile{}, project.NewError("configuration", fmt.Errorf("decode dac.lock: %w", err))
	}
	if err := Validate(value); err != nil {
		return Lockfile{}, err
	}
	return value, nil
}

// LoadOptional reads an existing lock file while distinguishing normal absence
// for the initial lock operation.
func LoadOptional(path string) (Lockfile, bool, error) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return Lockfile{}, false, nil
	} else if err != nil {
		return Lockfile{}, false, project.NewError("filesystem", err)
	}
	value, err := Load(path)
	return value, true, err
}

// Stage prepares lock metadata without changing accepted state. The caller can
// combine the returned file with downloaded artifacts in one transaction.
func Stage(path string, value Lockfile) (*atomic.File, error) {
	if err := Validate(value); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, project.NewError("filesystem", err)
	}
	file, err := atomic.Create(path, 0o644)
	if err != nil {
		return nil, project.NewError("filesystem", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		cleanupErr := file.Discard()
		return nil, project.NewError("filesystem", errors.Join(err, cleanupErr))
	}
	return file, nil
}

// Validate rejects values that could evade manifest comparison or make local
// verification ambiguous.
func Validate(value Lockfile) error {
	if value.Version != project.Version {
		return &project.Error{Kind: "configuration", Err: fmt.Errorf("unsupported dac.lock version %d", value.Version)}
	}
	if value.Files == nil {
		return &project.Error{Kind: "configuration", Err: errors.New("dac.lock must contain files")}
	}
	for name, file := range value.Files {
		if !manifest.ValidAssetName(name) || file.ResolvedURL == "" || filename.Clean(file.ResolvedFile) != file.ResolvedFile || file.Size < 0 {
			return &project.Error{Kind: "configuration", Asset: name, Err: errors.New("invalid lock entry")}
		}
		if err := manifest.ValidateResolvedURL(file.ResolvedURL); err != nil {
			return &project.Error{Kind: "configuration", Asset: name, Err: fmt.Errorf("invalid resolved_url: %w", err)}
		}
		digest, err := asset.NormalizeDigest(file.Digest)
		if err != nil {
			return &project.Error{Kind: "configuration", Asset: name, Err: fmt.Errorf("invalid digest: %w", err)}
		}
		file.Digest = digest
		value.Files[name] = file
	}
	return nil
}
