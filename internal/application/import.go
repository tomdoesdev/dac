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
	"github.com/tomdoesdev/dac/internal/jsonfile"
)

// ImportResult reports the objects installed from one cache bundle.
type ImportResult struct {
	Bundle      string `json:"bundle"`
	ItemCount   int    `json:"itemCount"`
	ObjectCount int    `json:"objectCount"`
	ByteCount   int64  `json:"byteCount"`
}

// Import installs objects into the local cache from a cache bundle or from a
// directory of digest-named files.
//
// The two are the same delivery in different wrappers -- a tar of objects named
// by digest, or those objects loose on a mounted share -- so they are one
// command rather than a command and a flag on an unrelated one. A digest is the
// only name a consumer can check, which is why both forms use it.
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
	return service.importBundle(ctx, absolute)
}

// importBundle installs the objects one cache bundle carries.
func (service *Service) importBundle(ctx context.Context, absolute string) (ImportResult, error) {
	file, err := os.Open(absolute)
	if err != nil {
		return ImportResult{}, fault.Wrap("import_read_failed", "DAC could not read the cache bundle.", err)
	}
	defer func() { _ = file.Close() }()

	reader := tar.NewReader(file)
	index, objects, err := readBundleIndex(reader)
	if err != nil {
		return ImportResult{}, invalidBundle(err)
	}
	result := ImportResult{Bundle: absolute, ItemCount: len(index.Items)}
	seen := make(map[string]struct{}, len(objects))
	for {
		if err := ctx.Err(); err != nil {
			return ImportResult{}, networkError(err)
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ImportResult{}, invalidBundle(err)
		}
		object, exists := objects[header.Name]
		if !exists {
			return ImportResult{}, invalidBundle(fmt.Errorf("bundle has unexpected file %q", header.Name))
		}
		if _, duplicate := seen[header.Name]; duplicate {
			return ImportResult{}, invalidBundle(fmt.Errorf("bundle has duplicate file %q", header.Name))
		}
		if !regularTarFile(header) {
			return ImportResult{}, invalidBundle(fmt.Errorf("bundle file %q is not a regular file", header.Name))
		}
		if header.Size != object.Size {
			return ImportResult{}, invalidBundle(fmt.Errorf("bundle file %q has size %d, not %d", header.Name, header.Size, object.Size))
		}
		// Hold the digest lock because PutExact assumes that its caller owns it.
		err = service.Store.WithLock(ctx, object.Digest, func() error {
			_, putErr := service.Store.Put(ctx, reader, PutExact(object))
			return putErr
		})
		if err != nil {
			var content *ContentError
			switch {
			case errors.As(err, &content), errors.Is(err, io.ErrUnexpectedEOF):
				return ImportResult{}, invalidBundle(err)
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				return ImportResult{}, networkError(err)
			default:
				return ImportResult{}, fault.Wrap("cache_write_failed", "DAC could not write the cache object.", err)
			}
		}
		seen[header.Name] = struct{}{}
		result.ObjectCount++
		result.ByteCount += object.Size
	}
	if len(seen) != len(objects) {
		for path := range objects {
			if _, exists := seen[path]; !exists {
				return ImportResult{}, invalidBundle(fmt.Errorf("bundle file %q is missing", path))
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
// damaged bundle.
func (service *Service) importDirectory(ctx context.Context, absolute string) (ImportResult, error) {
	entries, err := os.ReadDir(absolute)
	if err != nil {
		return ImportResult{}, fault.Wrap("import_read_failed", "DAC could not read the import directory.", err)
	}
	result := ImportResult{Bundle: absolute}
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

// readBundleIndex reads and checks the first tar entry.
func readBundleIndex(reader *tar.Reader) (bundleIndex, map[string]Object, error) {
	header, err := reader.Next()
	if err != nil {
		return bundleIndex{}, nil, err
	}
	if header.Name != bundleIndexPath || !regularTarFile(header) {
		return bundleIndex{}, nil, errors.New("the first bundle entry must be a regular index.json file")
	}
	if header.Size > maximumIndexSize {
		return bundleIndex{}, nil, fmt.Errorf("bundle index is larger than %d bytes", maximumIndexSize)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return bundleIndex{}, nil, err
	}
	var index bundleIndex
	if err := jsonfile.DecodeStrict(data, &index); err != nil {
		return bundleIndex{}, nil, err
	}
	objects, err := validateBundleIndex(index)
	return index, objects, err
}

// regularTarFile accepts the current and historical regular-file type flags.
func regularTarFile(header *tar.Header) bool {
	return header.Typeflag == tar.TypeReg || header.Typeflag == 0
}

// invalidBundle gives all bundle format failures one stable command error.
func invalidBundle(err error) error {
	return fault.Wrap("bundle_invalid", "The cache bundle is invalid.", err)
}
