package application

import (
	"bytes"
	"context"
	"errors"
	"os"

	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/project"
)

// LockResult reports a lock operation.
type LockResult struct {
	ManifestDigest string  `json:"manifestDigest"`
	Assets         []Asset `json:"assets"`
	AssetCount     int     `json:"assetCount"`
	Changed        bool    `json:"changed"`
	// Drifted names the assets whose resolved bytes differ from the lock file
	// the command started with.
	Drifted []string `json:"drifted"`
}

// Lock resolves every manifest asset and writes a stable lock file.
func (service *Service) Lock(ctx context.Context, options NetworkOptions) (LockResult, error) {
	defer service.Reporter.Wait()
	manifest, err := service.readManifest()
	if err != nil {
		return LockResult{}, err
	}
	old, _, err := project.ReadLockIfPresent(service.LockPath)
	if err != nil {
		return LockResult{}, fault.Wrap("lock_invalid", "The lock file is invalid.", err)
	}
	names := manifest.Names()
	resolved, err := parallel(ctx, options.Concurrency, names, func(ctx context.Context, name string) (resolvedAsset, error) {
		value, status, err := service.resolve(ctx, name, manifest.Assets[name], old.Assets[name], options)
		if err != nil && ctx.Err() == nil {
			service.Reporter.Fail(name, err)
		}
		return resolvedAsset{lock: value, status: status}, err
	})
	if err != nil {
		return LockResult{}, err
	}
	assets := make(map[string]project.LockAsset, len(names))
	views := make([]Asset, len(names))
	for index, name := range names {
		assets[name] = resolved[index].lock
		views[index], err = service.assetView(name, manifest.Assets[name], resolved[index].lock, resolved[index].status)
		if err != nil {
			return LockResult{}, withAsset(err, name)
		}
	}
	lock, err := project.NewLock(manifest, assets)
	if err != nil {
		return LockResult{}, fault.Wrap("lock_invalid", "DAC created an invalid lock file.", err)
	}
	data, err := project.Marshal(lock)
	if err != nil {
		return LockResult{}, fault.Wrap("lock_write_failed", "DAC could not encode the lock file.", err)
	}
	existing, readErr := os.ReadFile(service.LockPath)
	changed := !bytes.Equal(existing, data)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return LockResult{}, fault.Wrap("lock_write_failed", "DAC could not read the lock file.", readErr)
	}
	drifted := make([]string, 0, len(names))
	for index, name := range names {
		if old.Assets[name] != resolved[index].lock {
			drifted = append(drifted, name)
		}
	}
	// Check reports drift instead of absorbing it. A scheduled job wants to know
	// that an origin moved, and a job that silently rewrote the lock to match
	// would destroy the evidence on its way to reporting success.
	if options.Check {
		if changed {
			return LockResult{}, &fault.Error{
				Code:    "lock_drift",
				Message: "The lock file does not match what the origins now serve.",
				Details: map[string]any{"assets": drifted},
			}
		}
		return LockResult{ManifestDigest: lock.ManifestDigest, Assets: views, AssetCount: len(views), Drifted: drifted}, nil
	}
	if changed {
		if err := project.WriteBytes(service.LockPath, data); err != nil {
			return LockResult{}, fault.Wrap("lock_write_failed", "DAC could not write the lock file.", err)
		}
	}
	return LockResult{ManifestDigest: lock.ManifestDigest, Assets: views, AssetCount: len(views), Changed: changed, Drifted: drifted}, nil
}

type resolvedAsset struct {
	lock   project.LockAsset
	status string
}
