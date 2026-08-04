// Package project reads and writes DAC project files.
package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"strings"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/filename"
	"github.com/tomdoesdev/dac/internal/jsonfile"
	"github.com/tomdoesdev/dac/internal/urlpolicy"
)

const (
	ManifestVersion = 2
	LockVersion     = 2
	fileMode        = 0o644
)

// Manifest defines the assets for one project.
//
// Both files key their assets by coordinate, so the version is part of the key
// rather than a field of the value. A project holds as many versions of an
// asset as it names, and nothing can change what a version means by editing the
// entry in place: a different version is a different key.
type Manifest struct {
	SchemaVersion int                        `json:"schemaVersion"`
	Assets        map[coord.Coordinate]Asset `json:"assets"`
}

// Asset defines one source.
type Asset struct {
	URL               string `json:"url"`
	Integrity         string `json:"integrity,omitempty"`
	AllowInsecureHTTP bool   `json:"allowInsecureHttp,omitempty"`
}

// Lock records the exact bytes for a manifest.
type Lock struct {
	LockVersion    int                            `json:"lockVersion"`
	ManifestDigest string                         `json:"manifestDigest"`
	Assets         map[coord.Coordinate]LockAsset `json:"assets"`
}

// LockAsset records one resolved asset.
//
// Filename is the name the origin gives the asset, which a cache path cannot
// carry: that path is a digest, and a digest is the right name for bytes and
// the wrong name for a tool that switches on an extension. It belongs here
// rather than beside the cached object because it describes the source and not
// the bytes -- two coordinates that resolve to the same object share one file
// in the cache and may well disagree about what it is called.
//
// It is advisory. Nothing decides anything by it, no other field is checked
// against it, and it is absent from a lock written before it existed, so it
// stays out of every comparison that asks whether a lock still describes a
// manifest.
type LockAsset struct {
	URL      string `json:"url"`
	Digest   string `json:"digest"`
	Size     int64  `json:"size"`
	ETag     string `json:"etag,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// VersionCollisionError reports two versions of one asset that name the same
// bytes.
//
// This is the failure that made versions meaningless: an asset added again at a
// new version with its old source still attached resolves to the bytes the old
// version already had, and the project ends up with two names for one thing and
// no way to tell which one anybody meant. A lock file never holds one, because
// Validate refuses it, so no command has to decide what to do about it.
type VersionCollisionError struct {
	Group    coord.Group
	Versions []string
	Digest   string
}

func (value *VersionCollisionError) Error() string {
	return fmt.Sprintf("asset %s versions %s name the same bytes (%s)",
		value.Group, strings.Join(value.Versions, " and "), value.Digest)
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
//
// DAC accepts the Subresource Integrity "sha256-<base64>" spelling on input but
// compares and writes only "sha256:<hex>", so normalization has to happen
// before the manifest digest is calculated. A manifest written by DAC always
// holds the canonical form.
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
		// A manifest read from a file has already parsed every key. One built in
		// memory has not, so this is what stops a command from writing a
		// coordinate that DAC would refuse to read back.
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
	}
	return nil
}

// Validate checks the lock schema, each locked asset, and that no two versions
// of one asset name the same bytes.
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
		// A name is the one field here that a caller is invited to use as a path
		// element, and a lock file is a text file somebody can edit. Checking it
		// against the form DAC writes is what stops an entry naming "../../etc"
		// from reaching a script that trusted the lock.
		if asset.Filename != "" && filename.Clean(asset.Filename) != asset.Filename {
			return fmt.Errorf("lock asset %q has an invalid filename", name)
		}
	}
	return CheckVersions(lock.Assets)
}

// CheckVersions reports two versions of one asset that resolved to the same
// bytes.
//
// Two coordinates naming one object is fine when they are different assets: a
// namespace exists so that two projects can vendor the same file, and the cache
// stores it once either way. Two versions of the same asset is the case that
// cannot be true, because one of the two versions is then a label somebody
// invented for bytes that already had one.
func CheckVersions(assets map[coord.Coordinate]LockAsset) error {
	seen := make(map[groupDigest]coord.Coordinate, len(assets))
	for _, name := range coord.Sorted(assets) {
		key := groupDigest{group: name.Group(), digest: assets[name].Digest}
		previous, exists := seen[key]
		if exists {
			return &VersionCollisionError{
				Group:    name.Group(),
				Versions: []string{previous.Version, name.Version},
				Digest:   key.digest,
			}
		}
		seen[key] = name
	}
	return nil
}

type groupDigest struct {
	group  coord.Group
	digest string
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
//
// It is the per-asset half of CheckLock, exported because a command deciding
// which assets it has to resolve and a command checking the whole project ask
// the same question. Two answers to it would mean a lock that one command
// rewrites and another rejects.
func Agrees(asset Asset, locked LockAsset, exists bool) bool {
	return disagreement(asset, locked, exists) == ""
}

// disagreement returns why a lock entry no longer describes a manifest asset,
// or an empty string when it still does. CheckLock reports the reason, so a
// stale lock says which part of the asset moved.
//
// The version is not compared here because it cannot differ: both entries are
// found by the same coordinate, and the coordinate carries it.
//
// The file name is not compared either, and that is deliberate rather than an
// omission. It is a label the origin supplied, the manifest holds nothing to
// compare it against, and every lock written before it existed carries none --
// so comparing it would report every asset of every existing project as stale
// over a field that decides nothing.
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

// File is one of the two project documents. The constraint keeps Marshal and
// Write from accepting anything that is not a project file.
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

// WriteBytes atomically writes project file bytes that a caller has already
// marshalled, so lock does not have to encode the same document twice.
func WriteBytes(path string, data []byte) error {
	return jsonfile.WriteAtomic(path, data, fileMode)
}

// WritePair writes matching project files and restores the manifest if the
// second write fails.
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
