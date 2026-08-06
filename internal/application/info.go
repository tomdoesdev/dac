package application

import (
	"net/url"
	"strings"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
)

// The state words an info result reports.
const (
	LockCurrent      = "current"
	LockMissing      = "missing"
	LockStale        = "stale"
	CacheCached      = "cached"
	CacheMissing     = "missing"
	CacheCorrupt     = "corrupt"
	CacheUnavailable = "unavailable"
	TrustTrusted     = "trusted"
	TrustUntrusted   = "untrusted"
	// TrustUnknown is what a service with no trust list to ask can say.
	TrustUnknown = "unknown"
)

// InfoOptions selects the assets for one inspection.
type InfoOptions struct {
	// Selection narrows the assets to one coordinate or one asset's versions.
	Selection Selection
}

// InfoAsset combines manifest, lock, and cache information.
type InfoAsset struct {
	Coordinate string `json:"coordinate"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	SourceURL  string `json:"sourceUrl"`
	// Host is the host the source URL names, and TrustStatus is whether DAC would download from it.
	Host        string `json:"host,omitempty"`
	TrustStatus string `json:"trustStatus"`
	CacheStatus string `json:"cacheStatus"`
	Integrity   string `json:"integrity,omitempty"`
	// Filename is what the origin calls this asset.
	Filename string `json:"filename,omitempty"`
	Digest   string `json:"digest,omitempty"`
	Size     *int64 `json:"size,omitempty"`
	Path     string `json:"path,omitempty"`
}

// InfoResult reports the selected project assets and their aggregate states.
type InfoResult struct {
	Assets  []InfoAsset `json:"assets"`
	Summary InfoSummary `json:"summary"`
}

// InfoSummary counts the states worth acting on.
type InfoSummary struct {
	AssetCount   int `json:"assetCount"`
	CachedCount  int `json:"cachedCount"`
	CorruptCount int `json:"corruptCount"`
	// UntrustedCount is how many assets a pull would refuse to download.
	UntrustedCount int    `json:"untrustedCount"`
	LockStatus     string `json:"lockStatus"`
}

// Info inspects manifest assets without network access.
func (service *Service) Info(options InfoOptions) (InfoResult, error) {
	manifest, err := service.readManifest()
	if err != nil {
		return InfoResult{}, err
	}
	names, err := selected(manifest.Assets, options.Selection)
	if err != nil {
		return InfoResult{}, err
	}
	lock, lockStatus, err := service.infoLock(manifest)
	if err != nil {
		return InfoResult{}, err
	}
	// A stale lock arrives here empty, so a project DAC cannot trust records nothing.
	service.note(lock.Assets)
	result := InfoResult{
		Assets:  make([]InfoAsset, 0, len(names)),
		Summary: InfoSummary{AssetCount: len(names), LockStatus: lockStatus},
	}
	for _, name := range names {
		asset, err := service.infoAsset(name, manifest.Assets[name], lock, lockStatus)
		if err != nil {
			return InfoResult{}, err
		}
		switch asset.CacheStatus {
		case CacheCached:
			result.Summary.CachedCount++
		case CacheCorrupt:
			result.Summary.CorruptCount++
		}
		if asset.TrustStatus == TrustUntrusted {
			result.Summary.UntrustedCount++
		}
		result.Assets = append(result.Assets, asset)
	}
	return result, nil
}

// infoLock classifies a readable lock. A stale lock does not cause a command error.
func (service *Service) infoLock(manifest project.Manifest) (project.Lock, string, error) {
	lock, found, err := project.ReadLockIfPresent(service.LockPath)
	if err != nil {
		return project.Lock{}, "", fault.Wrap("lock_invalid", "The lock file is invalid.", err)
	}
	if !found {
		return project.Lock{}, LockMissing, nil
	}
	if err := project.CheckLock(manifest, lock); err != nil {
		return project.Lock{}, LockStale, nil
	}
	return lock, LockCurrent, nil
}

// infoAsset combines one manifest asset with its lock and cache states.
func (service *Service) infoAsset(name coord.Coordinate, source project.Asset, lock project.Lock, lockStatus string) (InfoAsset, error) {
	host := hostOf(source.URL)
	result := InfoAsset{
		Coordinate:  name.String(),
		Namespace:   name.Namespace,
		Name:        name.Name,
		Version:     name.Version,
		SourceURL:   source.URL,
		Host:        host,
		TrustStatus: service.trustStatus(host),
		CacheStatus: CacheUnavailable,
		Integrity:   source.Integrity,
	}
	// Trust is settled above the lock check because it comes from the manifest URL,
	// so unlike the cache fields it is knowable even when the lock is stale.
	if lockStatus != LockCurrent {
		return result, nil
	}
	view, err := service.assetView(name, source, lock.Assets[name], "")
	if err != nil {
		return InfoAsset{}, withAsset(err, name.String())
	}
	result.Filename = view.Filename
	result.Digest = view.Digest
	result.Size = &view.Size
	switch {
	case view.Corrupt:
		result.CacheStatus = CacheCorrupt
	case view.Cached:
		result.CacheStatus = CacheCached
		result.Path = view.Path
	default:
		result.CacheStatus = CacheMissing
	}
	return result, nil
}

// trustStatus reports whether a download would be allowed to reach a host.
// A service with no list to ask says so rather than saying no, because reporting
// every host as untrusted would be the wrong answer rather than a missing one.
func (service *Service) trustStatus(host string) string {
	if service.Trust == nil || host == "" {
		return TrustUnknown
	}
	if service.Trust.Trusted(host) {
		return TrustTrusted
	}
	return TrustUntrusted
}

// hostOf names the host a source URL points at.
// A manifest URL has already passed the URL policy, so a value this cannot parse
// is one nothing will download from anyway.
func hostOf(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}
