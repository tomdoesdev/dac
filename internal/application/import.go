package application

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/jsonfile"
)

// ImportResult reports the objects installed from one cache bundle.
type ImportResult struct {
	Bundle      string `json:"bundle"`
	ItemCount   int    `json:"itemCount"`
	ObjectCount int    `json:"objectCount"`
	ByteCount   int64  `json:"byteCount"`
}

// Import validates one cache bundle and installs its objects in the local cache.
func (service *Service) Import(ctx context.Context, bundlePath string) (ImportResult, error) {
	absolute, err := filepath.Abs(bundlePath)
	if err != nil {
		return ImportResult{}, fault.Wrap("import_read_failed", "DAC could not resolve the bundle path.", err)
	}
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
