package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/digest"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
)

// CheckResult reports project consistency and the cost of optional upstream validation.
type CheckResult struct {
	ManifestDigest string `json:"manifestDigest"`
	AssetCount     int    `json:"assetCount"`
	Upstream       bool   `json:"upstream"`
	Downloaded     int    `json:"downloaded"`
}

// CheckOptions controls one project check.
type CheckOptions struct {
	Concurrency int
	MaxSize     int64
	Upstream    bool
}

// Check strictly compares the project files and optionally confirms that the
// locked bytes are still what every origin serves. It never writes project,
// cache, or catalog state.
func (service *Service) Check(ctx context.Context, options CheckOptions) (CheckResult, error) {
	manifest, lock, err := service.readProjectFiles()
	if err != nil {
		return CheckResult{}, err
	}
	result := CheckResult{
		ManifestDigest: lock.ManifestDigest,
		AssetCount:     len(manifest.Assets),
		Upstream:       options.Upstream,
	}
	if !options.Upstream {
		return result, nil
	}

	defer service.Reporter.Wait()
	names := manifest.Coordinates()
	service.Reporter.Plan(coord.Strings(names))
	checks, err := parallel(ctx, options.Concurrency, names, func(ctx context.Context, name coord.Coordinate) (upstreamCheck, error) {
		checked, checkErr := service.checkUpstreamAsset(ctx, name, manifest.Assets[name], lock.Assets[name], options.MaxSize)
		if checkErr != nil && ctx.Err() == nil {
			service.Reporter.Fail(name.String(), checkErr)
		}
		return checked, checkErr
	})
	if err != nil {
		return CheckResult{}, err
	}

	drifted := make([]string, 0)
	for index, checked := range checks {
		if checked.downloaded {
			result.Downloaded++
		}
		if checked.drifted {
			drifted = append(drifted, names[index].String())
		}
	}
	if len(drifted) > 0 {
		return CheckResult{}, &fault.Error{
			Code:    "lock_drift",
			Message: "The lock file does not match what the origins now serve.",
			Details: map[string]any{"assets": drifted, "downloaded": result.Downloaded},
		}
	}
	return result, nil
}

type upstreamCheck struct {
	downloaded bool
	drifted    bool
}

// checkUpstreamAsset uses stored validators as cautious change hints and
// downloads whenever HEAD cannot positively repeat every hint DAC already knew.
func (service *Service) checkUpstreamAsset(ctx context.Context, name coord.Coordinate, source project.Asset, locked project.LockAsset, maxSize int64) (upstreamCheck, error) {
	if locked.ETag == "" && locked.LastModified == "" {
		return service.downloadAndCheck(ctx, name, source, locked, maxSize)
	}
	if service.Prober == nil {
		return upstreamCheck{}, fault.New("network_error", "No upstream prober is configured.")
	}
	probe, err := service.Prober.Probe(ctx, ProbeRequest{
		URL:               source.URL,
		AllowInsecureHTTP: source.AllowInsecureHTTP,
	})
	if err != nil {
		if headMayFallBack(err) {
			return service.downloadAndCheck(ctx, name, source, locked, maxSize)
		}
		return upstreamCheck{}, networkError(err)
	}
	if validatorsChanged(locked, probe) || (probe.Length != Unknown && probe.Length != locked.Size) {
		return service.downloadAndCheck(ctx, name, source, locked, maxSize)
	}
	return upstreamCheck{}, nil
}

// validatorsChanged compares only metadata already stored in the lock. New
// validators are useful to a later pull, but are not evidence that bytes moved.
func validatorsChanged(locked project.LockAsset, probe *ProbeResponse) bool {
	return (locked.ETag != "" && locked.ETag != probe.ETag) ||
		(locked.LastModified != "" && locked.LastModified != probe.LastModified)
}

// headMayFallBack recognizes origins that explicitly do not implement HEAD.
// Other request failures keep their normal network classification.
func headMayFallBack(err error) bool {
	var detail RequestDetail
	if !errors.As(err, &detail) {
		return false
	}
	status := detail.StatusCode()
	return status == http.StatusMethodNotAllowed || status == http.StatusNotImplemented
}

// downloadAndCheck streams origin bytes into a hash and discards them. The
// bounded reader preserves the same maximum-size policy as ordinary downloads
// without involving the object store.
func (service *Service) downloadAndCheck(ctx context.Context, name coord.Coordinate, source project.Asset, locked project.LockAsset, maxSize int64) (upstreamCheck, error) {
	response, err := service.fetch(ctx, source, project.LockAsset{})
	if err != nil {
		return upstreamCheck{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.NotModified {
		return upstreamCheck{}, networkError(errors.New("an unconditional asset request returned Not Modified"))
	}
	if maxSize > 0 && response.Length > maxSize {
		return upstreamCheck{}, contentError(fmt.Errorf("%w of %d bytes: the origin declared %d", ErrTooLarge, maxSize, response.Length))
	}

	service.Reporter.Start(name.String(), response.Length)
	hasher := sha256.New()
	reader := io.Reader(&transferReader{name: name.String(), reader: response.Body, reporter: service.Reporter})
	if maxSize > 0 {
		reader = io.LimitReader(reader, maxSize+1)
	}
	count, copyErr := io.Copy(hasher, reader)
	if copyErr != nil {
		return upstreamCheck{}, contentError(copyErr)
	}
	if maxSize > 0 && count > maxSize {
		return upstreamCheck{}, contentError(fmt.Errorf("%w of %d bytes", ErrTooLarge, maxSize))
	}
	actualDigest := digest.Prefix + hex.EncodeToString(hasher.Sum(nil))
	drifted := actualDigest != locked.Digest || count != locked.Size
	status := "checked"
	if drifted {
		status = "drifted"
	}
	service.Reporter.Done(name.String(), status)
	return upstreamCheck{downloaded: true, drifted: drifted}, nil
}
