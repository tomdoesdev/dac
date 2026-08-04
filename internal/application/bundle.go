package application

import (
	"fmt"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
)

const (
	bundleSchemaVersion = 2
	bundleIndexPath     = "index.json"
	maximumIndexSize    = 16 << 20
)

// BundleItem maps one project asset to one object in a cache bundle.
//
// It names the asset by its whole coordinate rather than by parts. The parts
// exist for a consumer reading a command result; an index DAC writes and reads
// back has no use for three fields that could contradict each other.
type BundleItem struct {
	Coordinate string `json:"coordinate"`
	SourceURL  string `json:"sourceUrl"`
	File       string `json:"file"`
	Digest     string `json:"digest"`
	Size       int64  `json:"size"`
}

// bundleIndex is the root document in a DAC cache bundle.
//
// The layout uses the OCI index name and content-addressed blob paths. DAC uses
// one direct item list because a cache bundle does not need OCI manifests.
type bundleIndex struct {
	SchemaVersion int          `json:"schemaVersion"`
	Items         []BundleItem `json:"items"`
}

// bundleBlobPath returns the only valid tar path for one object.
func bundleBlobPath(value string) (string, error) {
	hexValue, err := digest.Hex(value)
	if err != nil {
		return "", err
	}
	return "blobs/sha256/" + hexValue, nil
}

// validateBundleIndex checks item metadata and returns each unique object.
func validateBundleIndex(index bundleIndex) (map[string]Object, error) {
	if index.SchemaVersion != bundleSchemaVersion {
		return nil, fmt.Errorf("unsupported bundle schema version %d", index.SchemaVersion)
	}
	if index.Items == nil {
		return nil, fmt.Errorf("bundle items must be an array")
	}
	coordinates := make(map[coord.Coordinate]struct{}, len(index.Items))
	objects := make(map[string]Object, len(index.Items))
	for position, item := range index.Items {
		coordinate, err := coord.Parse(item.Coordinate)
		if err != nil {
			return nil, fmt.Errorf("bundle item %d: %w", position, err)
		}
		if item.SourceURL == "" {
			return nil, fmt.Errorf("bundle item %d has no source URL", position)
		}
		if item.Size < 0 {
			return nil, fmt.Errorf("bundle item %d has a negative size", position)
		}
		expectedFile, err := bundleBlobPath(item.Digest)
		if err != nil {
			return nil, fmt.Errorf("bundle item %d has an invalid digest: %w", position, err)
		}
		if item.File != expectedFile {
			return nil, fmt.Errorf("bundle item %d has file %q, not %q", position, item.File, expectedFile)
		}
		if _, exists := coordinates[coordinate]; exists {
			return nil, fmt.Errorf("bundle has duplicate item %q", coordinate)
		}
		coordinates[coordinate] = struct{}{}
		object := Object{Digest: item.Digest, Size: item.Size}
		if previous, exists := objects[item.File]; exists && previous != object {
			return nil, fmt.Errorf("bundle file %q has inconsistent metadata", item.File)
		}
		objects[item.File] = object
	}
	return objects, nil
}
