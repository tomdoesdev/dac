package application

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/tom/dac/internal/fault"
)

// PackResult reports one dacpack.
type PackResult struct {
	Pack string `json:"pack"`
	AssetSummary
}

// packEntry joins the metadata one item publishes to the local object it was
// written from.
type packEntry struct {
	metadata PackItem
	asset    Asset
}

// Pack writes every locked asset to one tar file under the name its origin
// gave it.
//
// It is export with the files materialized. A cache bundle is for moving the
// cache between machines that both run DAC, and names everything by digest
// because that is all DAC needs. A dacpack is for handing a project's assets to
// something that is not DAC: extract it and there is a directory of real files
// with real extensions, plus the index that maps them back if DAC ever sees it
// again.
//
// The cost of that is duplication. Two coordinates that resolved to the same
// bytes share one object in the cache and one blob in a bundle, but they are
// two files here, because each is materialized under its own name and a file
// cannot be in two places. Packing a project whose assets overlap therefore
// costs more than exporting it.
func (service *Service) Pack(ctx context.Context, packPath string) (PackResult, error) {
	manifest, lock, err := service.readProject()
	if err != nil {
		return PackResult{}, err
	}
	absolute, err := filepath.Abs(packPath)
	if err != nil {
		return PackResult{}, fault.Wrap("pack_write_failed", "DAC could not resolve the dacpack path.", err)
	}
	entries := make([]packEntry, 0, len(manifest.Assets))
	for _, coordinate := range manifest.Coordinates() {
		if err := ctx.Err(); err != nil {
			return PackResult{}, networkError(err)
		}
		name := coordinate.String()
		locked := lock.Assets[coordinate]
		view, err := service.assetView(coordinate, manifest.Assets[coordinate], locked, "packed")
		if err != nil {
			return PackResult{}, withAsset(err, name)
		}
		if view.Corrupt {
			return PackResult{}, withAsset(&fault.Error{
				Code:    "cache_object_corrupt",
				Message: "The cache object does not match its digest. Run dac cache verify --repair, then dac pull.",
				Details: map[string]any{"expectedDigest": locked.Digest},
			}, name)
		}
		if !view.Cached {
			return PackResult{}, withAsset(fault.New("cache_object_invalid", "The cache object is missing. Run dac pull."), name)
		}
		file := packFileName(coordinate, locked)
		target, err := packFilePath(coordinate, file)
		if err != nil {
			return PackResult{}, withAsset(fault.Wrap("lock_invalid", "The lock file has an unusable file name.", err), name)
		}
		entries = append(entries, packEntry{
			metadata: PackItem{
				Coordinate: name,
				SourceURL:  locked.URL,
				File:       target,
				Filename:   file,
				Digest:     locked.Digest,
				Size:       locked.Size,
			},
			asset: view,
		})
	}
	if err := writePack(ctx, absolute, entries); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return PackResult{}, networkError(err)
		}
		if corrupted(err) {
			return PackResult{}, cacheReadError(err)
		}
		return PackResult{}, fault.Wrap("pack_write_failed", "DAC could not write the dacpack.", err)
	}
	assets := make([]Asset, 0, len(entries))
	for _, entry := range entries {
		assets = append(assets, entry.asset)
	}
	return PackResult{Pack: absolute, AssetSummary: collect(assets)}, nil
}

// writePack writes a complete archive through one temporary file, so an
// interrupted pack leaves no half-written archive where a whole one was.
func writePack(ctx context.Context, destination string, entries []packEntry) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".dac-pack-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()

	writer := tar.NewWriter(temporary)
	if err := writePackContents(ctx, writer, entries); err != nil {
		_ = writer.Close()
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Chmod(0o644); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, destination)
}

// writePackContents writes the index first and then one file per asset.
//
// The index leads for the same reason a bundle's does: a reader streaming the
// archive has to know what it is looking at before the first file arrives,
// which is what lets unpack check each file as it goes rather than buffering
// the archive to find out.
func writePackContents(ctx context.Context, writer *tar.Writer, entries []packEntry) error {
	metadata := make([]PackItem, 0, len(entries))
	for _, entry := range entries {
		metadata = append(metadata, entry.metadata)
	}
	indexData, err := json.MarshalIndent(packIndex{SchemaVersion: packSchemaVersion, Items: metadata}, "", "  ")
	if err != nil {
		return err
	}
	indexData = append(indexData, '\n')
	if err := writeTarHeader(writer, packIndexPath, int64(len(indexData))); err != nil {
		return err
	}
	if _, err := writer.Write(indexData); err != nil {
		return err
	}

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeTarHeader(writer, entry.metadata.File, entry.metadata.Size); err != nil {
			return err
		}
		// The same check export makes, for the same reason: these bytes were
		// vouched for by a stat against the sidecar rather than by reading them,
		// and this is the read that could prove otherwise.
		if err := copyBundleObject(ctx, writer, entry.asset.Path, Object{
			Digest: entry.metadata.Digest,
			Size:   entry.metadata.Size,
		}); err != nil {
			return err
		}
	}
	return nil
}
