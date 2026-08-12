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

// ResolvedFiles lists every managed filename this accepted state claims.
// Callers use it to decide which directory entries dac still owns instead of
// each rebuilding the same set from Files.
func (value Lockfile) ResolvedFiles() []string {
	result := make([]string, 0, len(value.Files))
	for _, file := range value.Files {
		result = append(result, file.ResolvedFile)
	}
	return result
}

// Load reads and validates a strict machine-authored lock file.
func Load(path string) (Lockfile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Lockfile{}, fault.NewConfigurationError(ErrNotFound, fault.WithRecovery(lockAllRecovery()))
	}
	if err != nil {
		return Lockfile{}, fault.NewFilesystemError(err)
	}
	var value Lockfile
	if err := strictjson.Unmarshal(data, &value); err != nil {
		return Lockfile{}, fault.NewConfigurationError(fmt.Errorf("%w: %w", ErrDecode, err))
	}
	return Normalize(value)
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

// Normalize validates a lock file and returns it with every digest in dac's
// canonical spelling, so later comparisons are byte equality. Reading is the
// only step that needs it; a value dac itself produced is already canonical.
func Normalize(value Lockfile) (Lockfile, error) {
	if err := Validate(value); err != nil {
		return Lockfile{}, err
	}
	files := make(map[string]Asset, len(value.Files))
	for name, file := range value.Files {
		// Validate has already proved both digests parse.
		file.Digest, _ = asset.NormalizeDigest(file.Digest)
		file.ConfigurationDigest, _ = asset.NormalizeDigest(file.ConfigurationDigest)
		files[name] = file
	}
	return Lockfile{Version: value.Version, Files: files}, nil
}

// Validate rejects values that could evade manifest comparison or make local
// verification ambiguous. It does not modify value.
func Validate(value Lockfile) error {
	if value.Version != Version {
		return fault.NewConfigurationError(fmt.Errorf("%w %d", ErrUnsupportedVersion, value.Version), fault.WithRecovery(lockAllRecovery()))
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
		if _, err := asset.NormalizeDigest(file.Digest); err != nil {
			return fault.NewConfigurationError(fmt.Errorf("%w: %w", ErrInvalidDigest, err), fault.WithAsset(name))
		}
		if _, err := asset.NormalizeDigest(file.ConfigurationDigest); err != nil {
			return fault.NewConfigurationError(fmt.Errorf("%w: %w", ErrInvalidConfigurationDigest, err), fault.WithAsset(name))
		}
	}
	return nil
}

// Exists reports whether a lock file is present without reading or validating
// it. It does not follow symlinks, so a link standing in for dac.lock counts
// as present and a create-only lock will refuse to replace it.
func Exists(path string) (bool, error) {
	if _, err := os.Lstat(path); err == nil {
		return true, nil
	} else if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else {
		return false, fault.NewFilesystemError(err)
	}
}
