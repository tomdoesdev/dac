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

	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fault"
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
// It is the cache, moved, with the files materialized. Unpack it, or extract it
// with tar, and there is a directory of real files with real extensions, plus
// an index recording what each one is and what it should hash to; hand it to
// cache import instead and the same bytes land in another machine's cache. One
// archive answers both, which is why DAC no longer writes a second one that
// only another DAC could read.
//
// The cost is duplication. Two coordinates that resolved to the same bytes
// share one object in the cache and arrive as two files here, because each is
// materialized under its own name and a file cannot be in two places. Packing a
// project whose assets overlap therefore costs more than the cache it came
// from.
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
// The index leads because a reader streaming the archive has to know what it is
// looking at before the first file arrives, which is what lets unpack and cache
// import check each file as it goes rather than buffering the archive to find
// out.
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
		// These bytes were vouched for by a stat against the sidecar rather than
		// by reading them, and this is the read that could prove otherwise.
		if err := copyPackObject(ctx, writer, entry.asset.Path, Object{
			Digest: entry.metadata.Digest,
			Size:   entry.metadata.Size,
		}); err != nil {
			return err
		}
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

// copyPackObject checks the exact bytes while it writes one file.
func copyPackObject(ctx context.Context, destination io.Writer, source string, expected Object) error {
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
