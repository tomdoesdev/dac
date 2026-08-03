package application

import (
	"github.com/tom/dac/internal/fault"
	"github.com/tom/dac/internal/project"
	"github.com/tom/dac/internal/rewrite"
)

const (
	lockCurrent      = "current"
	lockMissing      = "missing"
	lockStale        = "stale"
	cacheCached      = "cached"
	cacheMissing     = "missing"
	cacheUnavailable = "unavailable"
	requestAllowed   = "allowed"
	requestBlocked   = "blocked"
)

// InfoOptions selects the assets and rewrite config for one inspection.
type InfoOptions struct {
	Name     string
	Version  string
	Rewriter *rewrite.Config
}

// InfoAsset combines manifest, request, lock, and cache information.
type InfoAsset struct {
	Name          string `json:"name"`
	Version       string `json:"version"`
	SourceURL     string `json:"sourceUrl"`
	RequestURL    string `json:"requestUrl"`
	RequestStatus string `json:"requestStatus"`
	Rewritten     bool   `json:"rewritten"`
	CacheStatus   string `json:"cacheStatus"`
	Integrity     string `json:"integrity,omitempty"`
	Digest        string `json:"digest,omitempty"`
	Size          *int64 `json:"size,omitempty"`
	Path          string `json:"path,omitempty"`
}

// InfoResult reports the selected project assets and their aggregate states.
type InfoResult struct {
	Assets       []InfoAsset `json:"assets"`
	AssetCount   int         `json:"assetCount"`
	CachedCount  int         `json:"cachedCount"`
	AllowedCount int         `json:"allowedCount"`
	BlockedCount int         `json:"blockedCount"`
	LockStatus   string      `json:"lockStatus"`
}

// Info inspects manifest assets without network access. It ignores lock data
// when the lock is missing or stale because that data does not describe the manifest.
func (service *Service) Info(options InfoOptions) (InfoResult, error) {
	manifest, err := service.readManifest()
	if err != nil {
		return InfoResult{}, err
	}
	names, err := selectedNames(manifest, options.Name, options.Version)
	if err != nil {
		return InfoResult{}, err
	}
	lock, lockStatus, err := service.infoLock(manifest)
	if err != nil {
		return InfoResult{}, err
	}
	result := InfoResult{
		Assets:     make([]InfoAsset, 0, len(names)),
		AssetCount: len(names),
		LockStatus: lockStatus,
	}
	for _, name := range names {
		asset, err := service.infoAsset(name, manifest.Assets[name], lock, lockStatus, options.Rewriter)
		if err != nil {
			return InfoResult{}, err
		}
		if asset.CacheStatus == cacheCached {
			result.CachedCount++
		}
		if asset.RequestStatus == requestBlocked {
			result.BlockedCount++
		} else {
			result.AllowedCount++
		}
		result.Assets = append(result.Assets, asset)
	}
	return result, nil
}

// selectedNames returns all sorted names or one exact active coordinate.
func selectedNames(manifest project.Manifest, name, version string) ([]string, error) {
	if name == "" && version == "" {
		return manifest.Names(), nil
	}
	asset, exists := manifest.Assets[name]
	if !exists || asset.Version != version {
		return nil, fault.New("asset_unknown", "The project does not have this asset coordinate.")
	}
	return []string{name}, nil
}

// infoLock classifies a readable lock. A stale lock does not cause a command error.
func (service *Service) infoLock(manifest project.Manifest) (project.Lock, string, error) {
	lock, found, err := project.ReadLockIfPresent(service.LockPath)
	if err != nil {
		return project.Lock{}, "", fault.Wrap("lock_invalid", "The lock file is invalid.", err)
	}
	if !found {
		return project.Lock{}, lockMissing, nil
	}
	if err := project.CheckLock(manifest, lock); err != nil {
		return project.Lock{}, lockStale, nil
	}
	return lock, lockCurrent, nil
}

// infoAsset combines one manifest asset with its request and cache states.
func (service *Service) infoAsset(name string, source project.Asset, lock project.Lock, lockStatus string, config *rewrite.Config) (InfoAsset, error) {
	decision, err := config.Evaluate(source.URL)
	if err != nil {
		return InfoAsset{}, withAsset(fault.Wrap("rewrite_failed", "DAC could not apply the rewrite config.", err), name)
	}
	requestStatus := requestAllowed
	if decision.Blocked {
		requestStatus = requestBlocked
	}
	result := InfoAsset{
		Name:          name,
		Version:       source.Version,
		SourceURL:     source.URL,
		RequestURL:    decision.URL,
		RequestStatus: requestStatus,
		Rewritten:     decision.Rewritten,
		CacheStatus:   cacheUnavailable,
		Integrity:     source.Integrity,
	}
	if lockStatus != lockCurrent {
		return result, nil
	}
	view, err := service.assetView(name, source, lock.Assets[name], "")
	if err != nil {
		return InfoAsset{}, withAsset(err, name)
	}
	result.Digest = view.Digest
	result.Size = &view.Size
	result.CacheStatus = cacheMissing
	if view.Cached {
		result.CacheStatus = cacheCached
		result.Path = view.Path
	}
	return result, nil
}
