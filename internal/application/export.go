package application

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/tom/dac/internal/digest"
	"github.com/tom/dac/internal/fault"
)

// ExportResult reports one cache bundle.
type ExportResult struct {
	Bundle string `json:"bundle"`
	AssetSummary
}

// exportItem joins public metadata to the local object path used during export.
type exportItem struct {
	metadata BundleItem
	asset    Asset
}

// Export writes all locked objects and their metadata to one tar file.
func (service *Service) Export(ctx context.Context, bundlePath string) (ExportResult, error) {
	manifest, lock, err := service.readProject()
	if err != nil {
		return ExportResult{}, err
	}
	absolute, err := filepath.Abs(bundlePath)
	if err != nil {
		return ExportResult{}, fault.Wrap("export_write_failed", "DAC could not resolve the bundle path.", err)
	}
	items := make([]exportItem, 0, len(manifest.Assets))
	for _, coordinate := range manifest.Coordinates() {
		if err := ctx.Err(); err != nil {
			return ExportResult{}, networkError(err)
		}
		name := coordinate.String()
		locked := lock.Assets[coordinate]
		view, err := service.assetView(coordinate, manifest.Assets[coordinate], locked, "exported")
		if err != nil {
			return ExportResult{}, withAsset(err, name)
		}
		if view.Corrupt {
			return ExportResult{}, withAsset(&fault.Error{
				Code:    "cache_object_corrupt",
				Message: "The cache object does not match its digest. Run dac cache verify --repair, then dac pull.",
				Details: map[string]any{"expectedDigest": locked.Digest},
			}, name)
		}
		if !view.Cached {
			return ExportResult{}, withAsset(fault.New("cache_object_invalid", "The cache object is missing. Run dac pull."), name)
		}
		file, err := bundleBlobPath(locked.Digest)
		if err != nil {
			return ExportResult{}, withAsset(fault.Wrap("lock_invalid", "The lock file has an invalid digest.", err), name)
		}
		items = append(items, exportItem{
			metadata: BundleItem{
				Coordinate: name,
				SourceURL:  locked.URL,
				File:       file,
				Digest:     locked.Digest,
				Size:       locked.Size,
			},
			asset: view,
		})
	}
	if err := writeBundle(ctx, absolute, items); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ExportResult{}, networkError(err)
		}
		if corrupted(err) {
			return ExportResult{}, cacheReadError(err)
		}
		return ExportResult{}, fault.Wrap("export_write_failed", "DAC could not write the cache bundle.", err)
	}
	assets := make([]Asset, 0, len(items))
	for _, item := range items {
		assets = append(assets, item.asset)
	}
	return ExportResult{Bundle: absolute, AssetSummary: collect(assets)}, nil
}

// writeBundle writes a complete archive through one temporary file.
func writeBundle(ctx context.Context, destination string, items []exportItem) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".dac-export-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}()

	writer := tar.NewWriter(temporary)
	if err := writeBundleContents(ctx, writer, items); err != nil {
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

// writeBundleContents writes the index first and writes each unique blob once.
func writeBundleContents(ctx context.Context, writer *tar.Writer, items []exportItem) error {
	metadata := make([]BundleItem, 0, len(items))
	for _, item := range items {
		metadata = append(metadata, item.metadata)
	}
	indexData, err := json.MarshalIndent(bundleIndex{SchemaVersion: bundleSchemaVersion, Items: metadata}, "", "  ")
	if err != nil {
		return err
	}
	indexData = append(indexData, '\n')
	if err := writeTarHeader(writer, bundleIndexPath, int64(len(indexData))); err != nil {
		return err
	}
	if _, err := writer.Write(indexData); err != nil {
		return err
	}

	written := make(map[string]struct{}, len(items))
	for _, item := range items {
		if _, exists := written[item.metadata.File]; exists {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := writeTarHeader(writer, item.metadata.File, item.metadata.Size); err != nil {
			return err
		}
		if err := copyBundleObject(ctx, writer, item.asset.Path, Object{
			Digest: item.metadata.Digest,
			Size:   item.metadata.Size,
		}); err != nil {
			return err
		}
		written[item.metadata.File] = struct{}{}
	}
	return nil
}

// writeTarHeader writes one stable regular-file header.
func writeTarHeader(writer *tar.Writer, name string, size int64) error {
	return writer.WriteHeader(&tar.Header{
		Name:    name,
		Mode:    0o444,
		Size:    size,
		ModTime: time.Unix(0, 0),
		Format:  tar.FormatUSTAR,
	})
}

// copyBundleObject checks the exact bytes while it writes one blob.
func copyBundleObject(ctx context.Context, destination io.Writer, source string, expected Object) error {
	reader, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	hashValue := sha256.New()
	written, err := io.Copy(io.MultiWriter(destination, hashValue), &contextReader{ctx: ctx, reader: reader})
	if err != nil {
		return err
	}
	actual := digest.Prefix + hex.EncodeToString(hashValue.Sum(nil))
	if actual != expected.Digest || written != expected.Size {
		return &CorruptError{Digest: expected.Digest, ActualDigest: actual, Path: source}
	}
	return nil
}

// contextReader stops a long file copy after command cancellation.
type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

// Read checks the command context before it reads the next file block.
func (reader *contextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}
