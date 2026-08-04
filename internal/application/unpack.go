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

// UnpackResult reports the objects installed from one dacpack.
type UnpackResult struct {
	Pack        string `json:"pack"`
	ItemCount   int    `json:"itemCount"`
	ObjectCount int    `json:"objectCount"`
	ByteCount   int64  `json:"byteCount"`
}

// Unpack validates one dacpack and installs its files in the local cache.
//
// It writes into the cache rather than onto the filesystem, which is what the
// index is for: the files in the archive are named the way their origins name
// them, and the index says which cache object each one is. So a dacpack that
// somebody extracted with tar and a dacpack DAC unpacked leave two different
// useful things behind, and this is the second one -- a warm cache that dac
// pull can answer from without a request.
//
// A file arrives here under a name a remote server chose, so nothing in the
// archive is trusted to say where it goes. Every path is recomputed from the
// coordinate it belongs to before anything is read, and the objects land under
// their digests in any case, so the worst a hostile archive can do is fail.
func (service *Service) Unpack(ctx context.Context, packPath string) (UnpackResult, error) {
	absolute, err := filepath.Abs(packPath)
	if err != nil {
		return UnpackResult{}, fault.Wrap("unpack_read_failed", "DAC could not resolve the dacpack path.", err)
	}
	file, err := os.Open(absolute)
	if err != nil {
		return UnpackResult{}, fault.Wrap("unpack_read_failed", "DAC could not read the dacpack.", err)
	}
	defer func() { _ = file.Close() }()

	reader := tar.NewReader(file)
	index, objects, err := readPackIndex(reader)
	if err != nil {
		return UnpackResult{}, invalidPack(err)
	}
	result := UnpackResult{Pack: absolute, ItemCount: len(index.Items)}
	seen := make(map[string]struct{}, len(objects))
	for {
		if err := ctx.Err(); err != nil {
			return UnpackResult{}, networkError(err)
		}
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return UnpackResult{}, invalidPack(err)
		}
		object, exists := objects[header.Name]
		if !exists {
			return UnpackResult{}, invalidPack(fmt.Errorf("dacpack has unexpected file %q", header.Name))
		}
		if _, duplicate := seen[header.Name]; duplicate {
			return UnpackResult{}, invalidPack(fmt.Errorf("dacpack has duplicate file %q", header.Name))
		}
		if !regularTarFile(header) {
			return UnpackResult{}, invalidPack(fmt.Errorf("dacpack file %q is not a regular file", header.Name))
		}
		if header.Size != object.Size {
			return UnpackResult{}, invalidPack(fmt.Errorf("dacpack file %q has size %d, not %d", header.Name, header.Size, object.Size))
		}
		// Hold the digest lock because PutExact assumes that its caller owns it.
		// Two files in one dacpack can carry the same object, since a project can
		// name one set of bytes at two coordinates; the second install is the same
		// bytes written over themselves, which the store already does on every
		// install rather than skipping.
		err = service.Store.WithLock(ctx, object.Digest, func() error {
			_, putErr := service.Store.Put(ctx, reader, PutExact(object))
			return putErr
		})
		if err != nil {
			var content *ContentError
			switch {
			case errors.As(err, &content), errors.Is(err, io.ErrUnexpectedEOF):
				return UnpackResult{}, invalidPack(err)
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				return UnpackResult{}, networkError(err)
			default:
				return UnpackResult{}, fault.Wrap("cache_write_failed", "DAC could not write the cache object.", err)
			}
		}
		seen[header.Name] = struct{}{}
		result.ObjectCount++
		result.ByteCount += object.Size
	}
	if len(seen) != len(objects) {
		for path := range objects {
			if _, exists := seen[path]; !exists {
				return UnpackResult{}, invalidPack(fmt.Errorf("dacpack file %q is missing", path))
			}
		}
	}
	return result, nil
}

// readPackIndex reads and checks the first tar entry.
func readPackIndex(reader *tar.Reader) (packIndex, map[string]Object, error) {
	header, err := reader.Next()
	if err != nil {
		return packIndex{}, nil, err
	}
	if header.Name != packIndexPath || !regularTarFile(header) {
		return packIndex{}, nil, errors.New("the first dacpack entry must be a regular index.json file")
	}
	if header.Size > maximumIndexSize {
		return packIndex{}, nil, fmt.Errorf("dacpack index is larger than %d bytes", maximumIndexSize)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return packIndex{}, nil, err
	}
	var index packIndex
	if err := jsonfile.DecodeStrict(data, &index); err != nil {
		return packIndex{}, nil, err
	}
	objects, err := validatePackIndex(index)
	return index, objects, err
}

// invalidPack gives all dacpack format failures one stable command error.
func invalidPack(err error) error {
	return fault.Wrap("dacpack_invalid", "The dacpack is invalid.", err)
}
