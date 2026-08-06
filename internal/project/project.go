// Package project reads and writes DAC project files.
package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fs/util/filename"
	"github.com/tomdoesdev/dac/internal/jsonfile"
	"github.com/tomdoesdev/dac/internal/urlpolicy"
)

const (
	ManifestVersion = 2
	LockVersion     = 2
	fileMode        = 0o644
)

// Manifest defines the assets for one project.
// Both files key their assets by coordinate, so the version is part of the key rather than a field of the value.
type Manifest struct {
	SchemaVersion int                        `json:"schemaVersion"`
	Assets        map[coord.Coordinate]Asset `json:"assets"`
}

// Asset defines one source.
// Filename is the name the project chooses for the asset, which is the one thing about a file name that is a decision rather than an observation.
type Asset struct {
	URL               string `json:"url"`
	Integrity         string `json:"integrity,omitempty"`
	Filename          string `json:"filename,omitempty"`
	AllowInsecureHTTP bool   `json:"allowInsecureHttp,omitempty"`
}

// Lock records the exact bytes for a manifest.
type Lock struct {
	LockVersion    int                            `json:"lockVersion"`
	ManifestDigest string                         `json:"manifestDigest"`
	Assets         map[coord.Coordinate]LockAsset `json:"assets"`
}

// LockAsset records one resolved asset.
// Filename records the project or origin name that a digest path cannot preserve.
// It is advisory.
type LockAsset struct {
	URL      string `json:"url"`
	Digest   string `json:"digest"`
	Size     int64  `json:"size"`
	ETag     string `json:"etag,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// Empty returns matching empty project files.
func Empty() (Manifest, Lock) {
	manifest := Manifest{SchemaVersion: ManifestVersion, Assets: map[coord.Coordinate]Asset{}}
	manifestDigest, _ := manifest.Digest()
	return manifest, Lock{LockVersion: LockVersion, ManifestDigest: manifestDigest, Assets: map[coord.Coordinate]LockAsset{}}
}

// ReadManifest reads, normalizes, and validates a manifest.
func ReadManifest(path string) (Manifest, error) {
	var manifest Manifest
	if err := jsonfile.ReadStrict(path, &manifest); err != nil {
		return Manifest{}, err
	}
	if err := manifest.Normalize(); err != nil {
		return Manifest{}, err
	}
	return manifest, manifest.Validate()
}

// Normalize rewrites each integrity value into the canonical digest form.
// Normalize accepts SRI input but hashes and writes canonical SHA-256 values.
func (manifest Manifest) Normalize() error {
	for name, asset := range manifest.Assets {
		if asset.Integrity == "" {
			continue
		}
		canonical, err := digest.Canonical(asset.Integrity)
		if err != nil {
			return fmt.Errorf("asset %q has invalid integrity: %w", name, err)
		}
		asset.Integrity = canonical
		manifest.Assets[name] = asset
	}
	return nil
}

// ReadLock reads and validates a lock file.
func ReadLock(path string) (Lock, error) {
	var lock Lock
	if err := jsonfile.ReadStrict(path, &lock); err != nil {
		return Lock{}, err
	}
	return lock, lock.Validate()
}

// ReadLockIfPresent reads a lock file when it exists.
func ReadLockIfPresent(path string) (Lock, bool, error) {
	lock, err := ReadLock(path)
	if err == nil {
		return lock, true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return Lock{}, false, nil
	}
	return Lock{}, false, err
}

// Validate checks the manifest schema and each asset.
func (manifest Manifest) Validate() error {
	if manifest.SchemaVersion != ManifestVersion {
		return fmt.Errorf("unsupported manifest schema version %d: version %d keys each asset by <namespace>/<name>@<version> and carries no version field",
			manifest.SchemaVersion, ManifestVersion)
	}
	if manifest.Assets == nil {
		return errors.New("manifest assets must be an object")
	}
	for name, asset := range manifest.Assets {
		// A manifest read from a file has already parsed every key.
		if err := name.Validate(); err != nil {
			return fmt.Errorf("manifest asset: %w", err)
		}
		parsed, err := url.ParseRequestURI(asset.URL)
		if err != nil || !parsed.IsAbs() || parsed.Host == "" {
			return fmt.Errorf("asset %q has an invalid URL", name)
		}
		if err := urlpolicy.Check(parsed, asset.AllowInsecureHTTP); err != nil {
			return fmt.Errorf("asset %q: %w", name, err)
		}
		if asset.Integrity != "" {
			if _, err := digest.Hex(asset.Integrity); err != nil {
				return fmt.Errorf("asset %q has invalid integrity: %w", name, err)
			}
		}
		// A declared name can become an output path, so DAC rejects unsafe values.
		if asset.Filename != "" && filename.Clean(asset.Filename) != asset.Filename {
			return fmt.Errorf("asset %q has an invalid filename", name)
		}
	}
	return nil
}

// Validate checks the lock schema and each locked asset.
func (lock Lock) Validate() error {
	if lock.LockVersion != LockVersion {
		return fmt.Errorf("unsupported lock version %d: version %d keys each asset by <namespace>/<name>@<version> and carries no version field",
			lock.LockVersion, LockVersion)
	}
	if _, err := digest.Hex(lock.ManifestDigest); err != nil {
		return fmt.Errorf("invalid manifest digest: %w", err)
	}
	if lock.Assets == nil {
		return errors.New("lock assets must be an object")
	}
	for name, asset := range lock.Assets {
		if err := name.Validate(); err != nil {
			return fmt.Errorf("lock asset: %w", err)
		}
		if asset.URL == "" {
			return fmt.Errorf("lock asset %q is incomplete", name)
		}
		if _, err := digest.Hex(asset.Digest); err != nil {
			return fmt.Errorf("lock asset %q has an invalid digest: %w", name, err)
		}
		if asset.Size < 0 {
			return fmt.Errorf("lock asset %q has a negative size", name)
		}
		// A name is the one field here that a caller is invited to use as a path element, and a lock file is a text file somebody can edit.
		if asset.Filename != "" && filename.Clean(asset.Filename) != asset.Filename {
			return fmt.Errorf("lock asset %q has an invalid filename", name)
		}
	}
	return nil
}

// Digest returns the digest of the normalized manifest.
func (manifest Manifest) Digest() (string, error) {
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	return digest.Bytes(data), nil
}

// Coordinates returns the manifest assets in a stable order.
func (manifest Manifest) Coordinates() []coord.Coordinate {
	return coord.Sorted(manifest.Assets)
}

// Clone returns a manifest whose asset map can be changed independently.
func (manifest Manifest) Clone() Manifest {
	return Manifest{SchemaVersion: manifest.SchemaVersion, Assets: maps.Clone(manifest.Assets)}
}

// Agrees reports whether a lock entry still describes a manifest asset.
// Agrees lets reconciliation and full lock checks share one asset rule.
func Agrees(asset Asset, locked LockAsset, exists bool) bool {
	return disagreement(asset, locked, exists) == ""
}

// disagreement returns why a lock entry no longer describes a manifest asset, or an empty string when it still does.
// The version is not compared here because it cannot differ: both entries are found by the same coordinate, and the coordinate carries it.
// The file name is not compared either, and that is deliberate rather than an omission.
func disagreement(asset Asset, locked LockAsset, exists bool) string {
	switch {
	case !exists:
		return "is not locked"
	case locked.URL != asset.URL:
		return "does not match"
	case asset.Integrity != "" && locked.Digest != asset.Integrity:
		return "does not match its integrity"
	}
	return ""
}

// CheckLock checks that a lock agrees with a manifest.
func CheckLock(manifest Manifest, lock Lock) error {
	manifestDigest, err := manifest.Digest()
	if err != nil {
		return err
	}
	if lock.ManifestDigest != manifestDigest {
		return errors.New("lock manifest digest does not match")
	}
	if len(manifest.Assets) != len(lock.Assets) {
		return errors.New("lock asset coordinates do not match")
	}
	for name, asset := range manifest.Assets {
		locked, exists := lock.Assets[name]
		if reason := disagreement(asset, locked, exists); reason != "" {
			return fmt.Errorf("lock asset %q %s", name, reason)
		}
	}
	return nil
}

// NewLock builds a lock from resolved assets.
func NewLock(manifest Manifest, assets map[coord.Coordinate]LockAsset) (Lock, error) {
	manifestDigest, err := manifest.Digest()
	if err != nil {
		return Lock{}, err
	}
	lock := Lock{LockVersion: LockVersion, ManifestDigest: manifestDigest, Assets: assets}
	return lock, lock.Validate()
}

// File is one of the two project documents.
type File interface{ Manifest | Lock }

// Marshal returns stable bytes for one project file.
func Marshal[T File](value T) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// Write atomically writes one project file.
func Write[T File](path string, value T) error {
	data, err := Marshal(value)
	if err != nil {
		return err
	}
	return WriteBytes(path, data)
}

// WriteBytes atomically writes project file bytes that a caller has already marshalled, so lock does not have to encode the same document twice.
func WriteBytes(path string, data []byte) error {
	return jsonfile.WriteAtomic(path, data, fileMode)
}

// WritePair writes matching project files and restores the manifest if the second write fails.
func WritePair(manifestPath, lockPath string, manifest Manifest, lock Lock) error {
	manifestData, err := Marshal(manifest)
	if err != nil {
		return err
	}
	lockData, err := Marshal(lock)
	if err != nil {
		return err
	}
	previous, readErr := os.ReadFile(manifestPath)
	manifestExisted := readErr == nil
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return readErr
	}
	if err := WriteBytes(manifestPath, manifestData); err != nil {
		return err
	}
	if err := WriteBytes(lockPath, lockData); err != nil {
		var restoreErr error
		if manifestExisted {
			restoreErr = WriteBytes(manifestPath, previous)
		} else {
			restoreErr = os.Remove(manifestPath)
			if errors.Is(restoreErr, os.ErrNotExist) {
				restoreErr = nil
			}
		}
		return errors.Join(err, restoreErr)
	}
	return nil
}
