package application

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fault"
)

// ImportResult reports the objects installed from one source.
type ImportResult struct {
	Source    string `json:"source"`
	ItemCount int    `json:"itemCount"`
	// ObjectCount and ByteCount report what the cache gained rather than what
	// the source held. A dacpack materializes each asset separately, so two
	// coordinates that resolved to one object arrive as two files and become one
	// object here, which is the number an operator is asking about when they ask
	// what an import installed.
	ObjectCount int   `json:"objectCount"`
	ByteCount   int64 `json:"byteCount"`
}

// Import installs objects into the local cache from a dacpack or from a
// directory of digest-named files.
//
// The two are the same delivery in different wrappers -- a project's assets in
// a tar, or objects loose on a mounted share -- so they are one command rather
// than a command and a flag on an unrelated one.
//
// The archive is the one pack writes, which is also the one unpack reads. DAC
// used to have a second format for this half alone, and a bundle only ever
// reached another DAC: a machine that received one and did not run DAC had a
// tar full of files named by hash and no way to tell which was which. Reading
// the archive that carries the origins' names costs this command a validation
// the digest layout gave for free, and dacpack.go is where that is paid back.
func (service *Service) Import(ctx context.Context, sourcePath string) (ImportResult, error) {
	absolute, err := filepath.Abs(sourcePath)
	if err != nil {
		return ImportResult{}, fault.Wrap("import_read_failed", "DAC could not resolve the import path.", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return ImportResult{}, fault.Wrap("import_read_failed", "DAC could not read the import source.", err)
	}
	if info.IsDir() {
		return service.importDirectory(ctx, absolute)
	}
	return service.importPack(ctx, absolute)
}

// importPack installs the objects one dacpack carries.
//
// It reads the archive the way unpack does and writes it where unpack does not.
// Every check unpack makes about an entry is made here too -- an entry the index
// never listed, a repeat, a type that is not a regular file, a size that
// disagrees -- because an import is the half that runs on the machine with no
// way to check the delivery against anything else.
//
// The one thing it does not do is derive a filesystem path. Nothing in the
// archive names a place on this machine: the cache decides where an object
// lives, from the digest, which is the same reason a bundle's layout was safe.
func (service *Service) importPack(ctx context.Context, absolute string) (ImportResult, error) {
	file, err := os.Open(absolute)
	if err != nil {
		return ImportResult{}, fault.Wrap("import_read_failed", "DAC could not read the dacpack.", err)
	}
	defer func() { _ = file.Close() }()

	reader := tar.NewReader(file)
	index, targets, err := readPackIndex(reader)
	if err != nil {
		return ImportResult{}, invalidPack(err)
	}
	result := ImportResult{Source: absolute, ItemCount: len(index.Items)}
	seen := make(map[string]struct{}, len(targets))
	installed := make(map[string]struct{}, len(targets))
	for {
		if err := ctx.Err(); err != nil {
			return ImportResult{}, networkError(err)
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ImportResult{}, invalidPack(err)
		}
		// The entry's name is a lookup key and never a path. What it finds
		// carries the digest DAC checked, which is what decides where the bytes
		// go and what they must be.
		target, exists := targets[header.Name]
		if !exists {
			return ImportResult{}, invalidPack(fmt.Errorf("dacpack has unexpected file %q", header.Name))
		}
		if _, duplicate := seen[target.path]; duplicate {
			return ImportResult{}, invalidPack(fmt.Errorf("dacpack has duplicate file %q", target.path))
		}
		if !regularTarFile(header) {
			return ImportResult{}, invalidPack(fmt.Errorf("dacpack file %q is not a regular file", target.path))
		}
		if header.Size != target.object.Size {
			return ImportResult{}, invalidPack(fmt.Errorf("dacpack file %q has size %d, not %d", target.path, header.Size, target.object.Size))
		}
		// Hold the digest lock because PutExact assumes that its caller owns it.
		err = service.Store.WithLock(ctx, target.object.Digest, func() error {
			_, putErr := service.Store.Put(ctx, reader, PutExact(target.object))
			return putErr
		})
		if err != nil {
			var content *ContentError
			switch {
			case errors.As(err, &content), errors.Is(err, io.ErrUnexpectedEOF):
				return ImportResult{}, invalidPack(err)
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				return ImportResult{}, networkError(err)
			default:
				return ImportResult{}, fault.Wrap("cache_write_failed", "DAC could not write the cache object.", err)
			}
		}
		seen[target.path] = struct{}{}
		// A repeat of an object already installed is still read and still
		// checked -- the archive claims those bytes at that path, and an import
		// that skipped them would not have looked at the whole delivery -- but
		// the cache gained it once, so it is counted once.
		if _, duplicate := installed[target.object.Digest]; !duplicate {
			installed[target.object.Digest] = struct{}{}
			result.ObjectCount++
			result.ByteCount += target.object.Size
		}
	}
	if len(seen) != len(targets) {
		for path := range targets {
			if _, exists := seen[path]; !exists {
				return ImportResult{}, invalidPack(fmt.Errorf("dacpack file %q is missing", path))
			}
		}
	}
	return result, nil
}

// importDirectory installs every digest-named file in a directory.
//
// A distribution directory carries no index, so the file name is the whole
// claim about what is in it, and Put checks each object against the digest its
// name gives. A file DAC cannot read as a digest is skipped rather than
// refused: a share holding a README beside the objects is a share, not a
// damaged delivery.
func (service *Service) importDirectory(ctx context.Context, absolute string) (ImportResult, error) {
	entries, err := os.ReadDir(absolute)
	if err != nil {
		return ImportResult{}, fault.Wrap("import_read_failed", "DAC could not read the import directory.", err)
	}
	result := ImportResult{Source: absolute}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return ImportResult{}, networkError(err)
		}
		if entry.IsDir() {
			continue
		}
		value := digest.Prefix + entry.Name()
		if _, err := digest.Hex(value); err != nil {
			continue
		}
		result.ItemCount++
		size, err := service.importFile(ctx, filepath.Join(absolute, entry.Name()), value)
		if err != nil {
			return ImportResult{}, err
		}
		result.ObjectCount++
		result.ByteCount += size
	}
	return result, nil
}

// importFile installs one digest-named file and reports the bytes it held.
func (service *Service) importFile(ctx context.Context, path, value string) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fault.Wrap("import_read_failed", "DAC could not read an import file.", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return 0, fault.Wrap("import_read_failed", "DAC could not read an import file.", err)
	}
	object := Object{Digest: value, Size: info.Size()}
	err = service.Store.WithLock(ctx, value, func() error {
		_, putErr := service.Store.Put(ctx, file, PutExact(object))
		return putErr
	})
	if err != nil {
		var content *ContentError
		switch {
		case errors.As(err, &content), errors.Is(err, io.ErrUnexpectedEOF):
			return 0, fault.Wrap("import_content_mismatch", "An import file does not match the digest its name gives.", err)
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return 0, networkError(err)
		default:
			return 0, fault.Wrap("cache_write_failed", "DAC could not write the cache object.", err)
		}
	}
	return object.Size, nil
}
