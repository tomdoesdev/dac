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
	// Refreshed distinguishes file checks from checks that reached every origin.
	Refreshed bool `json:"refreshed"`
}

// VerifyOptions controls one project verification.
type VerifyOptions struct {
	Concurrency int
	MaxSize     int64
	Refresh     bool
}

// Verify checks that the manifest and lock file agree, and with Refresh that the origins still serve the locked bytes.
// The two failures stay separate on purpose.
// Resolving an asset means downloading it, so a refresh warms the cache as a side effect.
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
	// Observe current bytes so verify can report drift without writing the lock.
	reconciled, err := service.reconcile(ctx, manifest, lock, reconcileOptions{
		concurrency: options.Concurrency,
		maxSize:     options.MaxSize,
		mode:        resolveObserve,
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

// driftedFrom reports whether an origin now serves different bytes than the lock file recorded.
// It compares the bytes and nothing else.
func driftedFrom(locked, resolved project.LockAsset) bool {
	return locked.Digest != resolved.Digest || locked.Size != resolved.Size
}
