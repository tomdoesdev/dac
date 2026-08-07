package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
)

// CheckMode identifies the amount of upstream work one check performs.
type CheckMode uint8

const (
	// CheckFiles compares only the manifest and lock files.
	CheckFiles CheckMode = iota
	// CheckMetadata compares upstream HEAD metadata with the lock file.
	CheckMetadata
	// CheckBytes streams and hashes upstream bytes against the lock file.
	CheckBytes
)

// CheckResult reports project consistency and any upstream work performed.
type CheckResult struct {
	ManifestDigest string `json:"manifestDigest"`
	ProjectCount   int    `json:"projectCount"`
	AssetCount     int    `json:"assetCount"`
	Upstream       bool   `json:"upstream"`
	Verified       bool   `json:"verified"`
	Downloaded     int    `json:"downloaded"`
}

// CheckOptions controls one project check.
type CheckOptions struct {
	Concurrency int
	MaxSize     int64
	Mode        CheckMode
	Assets      []coord.Coordinate
}

// Check compares the project files, then optionally compares selected origin
// metadata or bytes. It never writes project, cache, or catalog state.
func (service *Service) Check(ctx context.Context, options CheckOptions) (CheckResult, error) {
	manifest, lock, err := service.readProjectFiles()
	if err != nil {
		return CheckResult{}, err
	}
	names, err := checkedCoordinates(manifest, options.Assets)
	if err != nil {
		return CheckResult{}, err
	}
	result := CheckResult{
		ManifestDigest: lock.ManifestDigest,
		ProjectCount:   len(manifest.Assets),
		AssetCount:     len(names),
		Upstream:       options.Mode != CheckFiles,
		Verified:       options.Mode == CheckBytes,
	}
	if options.Mode == CheckFiles {
		return result, nil
	}

	defer service.Reporter.Wait()
	service.Reporter.Plan(coord.Strings(names))
	checks, err := parallel(ctx, options.Concurrency, names, func(ctx context.Context, name coord.Coordinate) (assetCheck, error) {
		var checked assetCheck
		var checkErr error
		source, locked := manifest.Assets[name], lock.Assets[name]
		switch options.Mode {
		case CheckMetadata:
			checked, checkErr = service.checkMetadataAsset(ctx, source, locked)
		case CheckBytes:
			checked, checkErr = service.verifyAsset(ctx, name, source, locked, options.MaxSize)
		default:
			checkErr = fault.New("invalid_arguments", "The check mode is invalid.")
		}
		if checkErr != nil && ctx.Err() == nil {
			service.Reporter.Fail(name.String(), checkErr)
		}
		return checked, checkErr
	})
	if err != nil {
		return CheckResult{}, err
	}
	populateMismatchNames(names, checks)

	mismatches := make([]checkMismatch, 0)
	for _, checked := range checks {
		if checked.downloaded {
			result.Downloaded++
		}
		if checked.mismatch != nil {
			mismatches = append(mismatches, *checked.mismatch)
		}
	}
	if len(mismatches) > 0 {
		code, heading := "metadata_mismatch", "The upstream metadata does not match the lock file."
		if options.Mode == CheckBytes {
			code, heading = "lock_drift", "The upstream bytes do not match the lock file."
		}
		return CheckResult{}, mismatchError(code, heading, mismatches, result.Downloaded)
	}
	return result, nil
}

// checkedCoordinates validates explicit selections before any upstream request
// and returns them once in stable project order.
func checkedCoordinates(manifest project.Manifest, requested []coord.Coordinate) ([]coord.Coordinate, error) {
	if len(requested) == 0 {
		return manifest.Coordinates(), nil
	}
	wanted := make(map[coord.Coordinate]struct{}, len(requested))
	for _, name := range requested {
		if _, exists := manifest.Assets[name]; !exists {
			return nil, unknownCoordinate(name, manifest.Assets)
		}
		wanted[name] = struct{}{}
	}
	names := make([]coord.Coordinate, 0, len(wanted))
	for _, name := range manifest.Coordinates() {
		if _, selected := wanted[name]; selected {
			names = append(names, name)
		}
	}
	return names, nil
}

type assetCheck struct {
	downloaded bool
	mismatch   *checkMismatch
}

type checkMismatch struct {
	name        coord.Coordinate
	differences []checkDifference
}

type checkDifference struct {
	field    string
	expected any
	actual   any
}

// checkMetadataAsset compares only the metadata already promised by the lock.
// It intentionally never turns a changed hint into a body download.
func (service *Service) checkMetadataAsset(ctx context.Context, source project.Asset, locked project.LockAsset) (assetCheck, error) {
	if service.Prober == nil {
		return assetCheck{}, fault.New("network_error", "No upstream prober is configured.")
	}
	probe, err := service.Prober.Probe(ctx, ProbeRequest{
		URL: source.URL,
	})
	if err != nil {
		return assetCheck{}, networkError(err)
	}
	differences := metadataDifferences(locked, probe)
	if len(differences) == 0 {
		return assetCheck{}, nil
	}
	return assetCheck{mismatch: &checkMismatch{differences: differences}}, nil
}

// metadataDifferences ignores newly offered validators because the lock did not
// promise them, while a known length is always comparable to the locked bytes.
func metadataDifferences(locked project.LockAsset, probe *ProbeResponse) []checkDifference {
	differences := make([]checkDifference, 0, 3)
	if locked.ETag != "" && locked.ETag != probe.ETag {
		differences = append(differences, checkDifference{field: "etag", expected: locked.ETag, actual: probe.ETag})
	}
	if locked.LastModified != "" && locked.LastModified != probe.LastModified {
		differences = append(differences, checkDifference{field: "lastModified", expected: locked.LastModified, actual: probe.LastModified})
	}
	if probe.Length != Unknown && probe.Length != locked.Size {
		differences = append(differences, checkDifference{field: "size", expected: locked.Size, actual: probe.Length})
	}
	return differences
}

// verifyAsset streams origin bytes into a hash and discards them. The bounded
// reader preserves normal transfer limits without involving the object store.
func (service *Service) verifyAsset(ctx context.Context, name coord.Coordinate, source project.Asset, locked project.LockAsset, maxSize int64) (assetCheck, error) {
	response, err := service.fetch(ctx, source, project.LockAsset{})
	if err != nil {
		return assetCheck{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.NotModified {
		return assetCheck{}, networkError(errors.New("an unconditional asset request returned Not Modified"))
	}
	if maxSize > 0 && response.Length > maxSize {
		return assetCheck{}, contentError(fmt.Errorf("%w of %d bytes: the origin declared %d", ErrTooLarge, maxSize, response.Length))
	}

	service.Reporter.Start(name.String(), response.Length)
	hasher := sha256.New()
	reader := io.Reader(&transferReader{name: name.String(), reader: response.Body, reporter: service.Reporter})
	if maxSize > 0 {
		reader = io.LimitReader(reader, maxSize+1)
	}
	count, copyErr := io.Copy(hasher, reader)
	if copyErr != nil {
		return assetCheck{}, contentError(copyErr)
	}
	if maxSize > 0 && count > maxSize {
		return assetCheck{}, contentError(fmt.Errorf("%w of %d bytes", ErrTooLarge, maxSize))
	}
	actualDigest := digest.Prefix + hex.EncodeToString(hasher.Sum(nil))
	differences := contentDifferences(locked, actualDigest, count)
	status := "checked"
	if len(differences) > 0 {
		status = "drifted"
	}
	service.Reporter.Done(name.String(), status)
	if len(differences) == 0 {
		return assetCheck{downloaded: true}, nil
	}
	return assetCheck{downloaded: true, mismatch: &checkMismatch{differences: differences}}, nil
}

func contentDifferences(locked project.LockAsset, actualDigest string, actualSize int64) []checkDifference {
	differences := make([]checkDifference, 0, 2)
	if locked.Digest != actualDigest {
		differences = append(differences, checkDifference{field: "digest", expected: locked.Digest, actual: actualDigest})
	}
	if locked.Size != actualSize {
		differences = append(differences, checkDifference{field: "size", expected: locked.Size, actual: actualSize})
	}
	return differences
}

// mismatchError preserves machine-readable differences and puts the same
// coordinate-rich explanation in the human-readable error message.
func mismatchError(code, heading string, mismatches []checkMismatch, downloaded int) error {
	assets := make([]string, 0, len(mismatches))
	values := make([]map[string]any, 0, len(mismatches))
	lines := make([]string, 0, len(mismatches)+1)
	lines = append(lines, heading)
	for _, mismatch := range mismatches {
		assets = append(assets, mismatch.name.String())
		fields := make([]map[string]any, 0, len(mismatch.differences))
		parts := make([]string, 0, len(mismatch.differences))
		for _, difference := range mismatch.differences {
			fields = append(fields, map[string]any{
				"field": difference.field, "expected": difference.expected, "actual": difference.actual,
			})
			parts = append(parts, describeDifference(difference))
		}
		values = append(values, map[string]any{"coordinate": mismatch.name.String(), "fields": fields})
		lines = append(lines, fmt.Sprintf("- %s: %s", mismatch.name, strings.Join(parts, "; ")))
	}
	return &fault.Error{
		Code: code, Message: strings.Join(lines, "\n"),
		Details: map[string]any{"assets": assets, "mismatches": values, "downloaded": downloaded},
	}
}

func describeDifference(difference checkDifference) string {
	if difference.field == "size" {
		return fmt.Sprintf("size is %d bytes, expected %d bytes", difference.actual, difference.expected)
	}
	name := map[string]string{"etag": "ETag", "lastModified": "Last-Modified", "digest": "digest"}[difference.field]
	return fmt.Sprintf("%s is %q, expected %q", name, difference.actual, difference.expected)
}

// populateMismatchNames assigns the stable coordinate to each parallel result.
// Keeping it separate keeps workers independent of output ordering.
func populateMismatchNames(names []coord.Coordinate, checks []assetCheck) {
	for index := range checks {
		if checks[index].mismatch != nil {
			checks[index].mismatch.name = names[index]
		}
	}
}
