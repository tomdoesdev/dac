package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/tom/dac/internal/digest"
	"github.com/tom/dac/internal/fault"
)

// ExportResult reports one distribution directory.
type ExportResult struct {
	Directory string `json:"directory"`
	AssetSummary
}

// Export copies every locked object into a directory named by digest, so an
// isolated machine can populate its cache with pull --distdir.
func (service *Service) Export(ctx context.Context, directory string) (ExportResult, error) {
	manifest, lock, err := service.readProject()
	if err != nil {
		return ExportResult{}, err
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return ExportResult{}, fault.Wrap("export_write_failed", "DAC could not create the export directory.", err)
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return ExportResult{}, fault.Wrap("export_write_failed", "DAC could not resolve the export directory.", err)
	}
	var exported []Asset
	for _, name := range manifest.Names() {
		if err := ctx.Err(); err != nil {
			return ExportResult{}, networkError(err)
		}
		locked := lock.Assets[name]
		view, err := service.assetView(name, manifest.Assets[name], locked, "exported")
		if err != nil {
			return ExportResult{}, withAsset(err, name)
		}
		if view.Corrupt {
			return ExportResult{}, withAsset(&fault.Error{
				Code:    "cache_object_corrupt",
				Message: "The cache object does not match its digest and must not be exported. Run dac cache verify --repair, then dac pull.",
				Details: map[string]any{"expectedDigest": locked.Digest},
			}, name)
		}
		if !view.Cached {
			return ExportResult{}, withAsset(fault.New("cache_object_invalid", "The cache object is missing. Run dac pull."), name)
		}
		hexValue, err := digest.Hex(locked.Digest)
		if err != nil {
			return ExportResult{}, withAsset(fault.Wrap("lock_invalid", "The lock file has an invalid digest.", err), name)
		}
		if err := copyObject(view.Path, filepath.Join(absolute, hexValue), locked.Digest); err != nil {
			if corrupted(err) {
				return ExportResult{}, withAsset(cacheReadError(err), name)
			}
			return ExportResult{}, withAsset(fault.Wrap("export_write_failed", "DAC could not write the export file.", err), name)
		}
		exported = append(exported, view)
	}
	return ExportResult{Directory: absolute, AssetSummary: collect(exported)}, nil
}

// copyObject writes one cache object into a distribution directory, hashing it
// on the way through.
//
// The bytes are already streaming, so the digest costs almost nothing, and it
// closes the gap between the check that cleared this object and the read that
// copies it. A bundle is the one artifact that carries cache damage to machines
// that cannot tell where it came from: the digest in the file name is the only
// provenance a consumer gets, so it had better be true when it is written.
func copyObject(source, destination, expected string) error {
	if _, err := os.Stat(destination); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	reader, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".dac-export-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()
	hashValue := sha256.New()
	if _, err := io.Copy(io.MultiWriter(temporary, hashValue), reader); err != nil {
		return err
	}
	if actual := digest.Prefix + hex.EncodeToString(hashValue.Sum(nil)); actual != expected {
		return &CorruptError{Digest: expected, ActualDigest: actual, Path: source}
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Chmod(0o444); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, destination)
}
