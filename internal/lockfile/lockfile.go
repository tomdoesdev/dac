package lockfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/kit/fs/atomic"
	"github.com/tomdoesdev/kit/fs/util/filename"
	"github.com/tomdoesdev/kit/strictjson"
)

// Version is the current machine-authored lock format.
const Version = 2

// Lockfile is machine-owned accepted state. It stores only the resolution and
// bytes, never request credentials or response metadata.
type Lockfile struct {
	Version int              `json:"version"`
	Files   map[string]Asset `json:"files"`
}

// Asset records the accepted resolution and bytes for one manifest asset.
type Asset struct {
	ResolvedURL         string `json:"resolved_url"`
	ResolvedFile        string `json:"resolved_file"`
	ConfigurationDigest string `json:"configuration_digest"`
	Digest              string `json:"digest"`
	Size                int64  `json:"size"`
}

// Load reads and validates a strict machine-authored lock file.
func Load(path string) (Lockfile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Lockfile{}, fault.NewConfigurationError(ErrNotFound, fault.WithHint("run `dac lock --all`"))
	}
	if err != nil {
		return Lockfile{}, fault.NewFilesystemError(err)
	}
	var value Lockfile
	if err := strictjson.Unmarshal(data, &value); err != nil {
		return Lockfile{}, fault.NewConfigurationError(fmt.Errorf("%w: %w", ErrDecode, err))
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
		return Lockfile{}, false, fault.NewFilesystemError(err)
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
		return nil, fault.NewFilesystemError(err)
	}
	file, err := atomic.Create(path, 0o644)
	if err != nil {
		return nil, fault.NewFilesystemError(err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		cleanupErr := file.Discard()
		return nil, fault.NewFilesystemError(errors.Join(err, cleanupErr))
	}
	return file, nil
}

// Validate rejects values that could evade manifest comparison or make local
// verification ambiguous.
func Validate(value Lockfile) error {
	if value.Version != Version {
		return fault.NewConfigurationError(fmt.Errorf("%w %d", ErrUnsupportedVersion, value.Version), fault.WithHint("run `dac lock --all`"))
	}
	if value.Files == nil {
		return fault.NewConfigurationError(ErrMissingFiles)
	}
	for name, file := range value.Files {
		if !manifest.ValidAssetName(name) || file.ResolvedURL == "" || filename.Clean(file.ResolvedFile) != file.ResolvedFile || file.Size < 0 {
			return fault.NewConfigurationError(ErrInvalidEntry, fault.WithAsset(name))
		}
		if err := manifest.ValidateResolvedURL(file.ResolvedURL); err != nil {
			return fault.NewConfigurationError(fmt.Errorf("%w: %w", ErrInvalidResolvedURL, err), fault.WithAsset(name))
		}
		digest, err := asset.NormalizeDigest(file.Digest)
		if err != nil {
			return fault.NewConfigurationError(fmt.Errorf("%w: %w", ErrInvalidDigest, err), fault.WithAsset(name))
		}
		file.Digest = digest
		configurationDigest, err := asset.NormalizeDigest(file.ConfigurationDigest)
		if err != nil {
			return fault.NewConfigurationError(fmt.Errorf("%w: %w", ErrInvalidConfigurationDigest, err), fault.WithAsset(name))
		}
		file.ConfigurationDigest = configurationDigest
		value.Files[name] = file
	}
	return nil
}
