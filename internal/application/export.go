package application

import (
	"context"
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
		if !view.Cached {
			return ExportResult{}, withAsset(fault.New("cache_object_invalid", "The cache object is missing. Run dac pull."), name)
		}
		hexValue, err := digest.Hex(locked.Digest)
		if err != nil {
			return ExportResult{}, withAsset(fault.Wrap("lock_invalid", "The lock file has an invalid digest.", err), name)
		}
		if err := copyFile(view.Path, filepath.Join(absolute, hexValue)); err != nil {
			return ExportResult{}, withAsset(fault.Wrap("export_write_failed", "DAC could not write the export file.", err), name)
		}
		exported = append(exported, view)
	}
	return ExportResult{Directory: absolute, AssetSummary: collect(exported)}, nil
}

func copyFile(source, destination string) error {
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
	if _, err := io.Copy(temporary, reader); err != nil {
		return err
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
