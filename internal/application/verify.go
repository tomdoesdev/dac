package application

import (
	"context"

	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
)

// VerifyResult reports matching project files.
type VerifyResult struct {
	ManifestDigest string `json:"manifestDigest"`
	AssetCount     int    `json:"assetCount"`
	// Refreshed reports that the check reached the origins rather than stopping
	// at the two files, which is the difference between a lock that is
	// internally consistent and one the publishers still stand behind.
	Refreshed bool `json:"refreshed"`
}

// VerifyOptions controls one project verification.
type VerifyOptions struct {
	Concurrency int
	MaxSize     int64
	Refresh     bool
}

// Verify checks that the manifest and lock file agree, and with Refresh that
// the origins still serve the locked bytes.
//
// The two failures stay separate on purpose. A manifest someone edited without
// locking it is lock_stale and the fix is a pull; an origin that replaced the
// bytes behind a stable URL is lock_drift and the fix is a decision. Verify
// writes nothing either way: a scheduled job that rewrote the lock to match
// what it found would destroy the evidence on its way to reporting success.
//
// Resolving an asset means downloading it, so a refresh warms the cache as a
// side effect. Nothing depends on that, but a large project should expect the
// transfer rather than discover it. For a drifted asset that includes the bytes
// the origin now serves, stored under their own digest: a content-addressed
// name cannot collide with the locked object, so the good bytes stay where they
// are and the new ones age out of the cache like anything else.
func (service *Service) Verify(ctx context.Context, options VerifyOptions) (VerifyResult, error) {
	manifest, lock, err := service.readProject()
	if err != nil {
		return VerifyResult{}, err
	}
	result := VerifyResult{ManifestDigest: lock.ManifestDigest, AssetCount: len(manifest.Assets)}
	if !options.Refresh {
		return result, nil
	}
	defer service.Reporter.Wait()
	result.Refreshed = true
	// Observe the current bytes without pin checks or rebind checks. This mode
	// lets verify report all drift as one result.
	reconciled, err := service.reconcile(ctx, manifest, lock, reconcileOptions{
		concurrency: options.Concurrency,
		maxSize:     options.MaxSize,
		mode:        resolveObserve,
		allowRebind: true,
	})
	if err != nil {
		return VerifyResult{}, err
	}
	drifted := make([]string, 0, len(manifest.Assets))
	for _, name := range manifest.Coordinates() {
		if driftedFrom(lock.Assets[name], reconciled.assets[name]) {
			drifted = append(drifted, name.String())
		}
	}
	if len(drifted) > 0 {
		return VerifyResult{}, &fault.Error{
			Code:    "lock_drift",
			Message: "The lock file does not match what the origins now serve.",
			Details: map[string]any{"assets": drifted},
		}
	}
	return result, nil
}

// driftedFrom reports whether an origin now serves different bytes than the
// lock file recorded.
//
// It compares the bytes and nothing else. An ETag is a cache hint that a server
// is free to rotate over identical content, and reporting that as drift would
// wake somebody up for a header. The URL and version cannot have moved here:
// they come from the manifest, which verify has already checked the lock agrees
// with.
func driftedFrom(locked, resolved project.LockAsset) bool {
	return locked.Digest != resolved.Digest || locked.Size != resolved.Size
}
