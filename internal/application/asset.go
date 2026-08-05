package application

import (
	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/project"
)

// AssetSummary reports the assets that a command handled.
type AssetSummary struct {
	Assets     []Asset `json:"assets"`
	AssetCount int     `json:"assetCount"`
	ByteCount  int64   `json:"byteCount"`
}

// collect summarizes the assets a command touched.
func collect(assets []Asset) AssetSummary {
	if assets == nil {
		assets = []Asset{}
	}
	summary := AssetSummary{Assets: assets, AssetCount: len(assets)}
	for _, asset := range assets {
		summary.ByteCount += asset.Size
	}
	return summary
}

// Asset describes one command result.
// It reports the coordinate whole and in parts.
type Asset struct {
	Coordinate string `json:"coordinate"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	URL        string `json:"url"`
	Integrity  string `json:"integrity,omitempty"`
	// Filename is what this asset is called: the name the manifest declares, or what the origin calls it when the manifest declares none.
	Filename string `json:"filename,omitempty"`
	Digest   string `json:"digest"`
	Size     int64  `json:"size"`
	Cached   bool   `json:"cached"`
	// Corrupt marks an object the cache holds but cannot vouch for.
	Corrupt bool   `json:"corrupt,omitempty"`
	Path    string `json:"path,omitempty"`
	Status  string `json:"status,omitempty"`
}

// assetView reports the state of one asset.
func (service *Service) assetView(name coord.Coordinate, source project.Asset, locked project.LockAsset, status string) (Asset, error) {
	view := Asset{
		Coordinate: name.String(),
		Namespace:  name.Namespace,
		Name:       name.Name,
		Version:    name.Version,
		URL:        source.URL,
		Integrity:  source.Integrity,
		Filename:   locked.Filename,
		Digest:     locked.Digest,
		Size:       locked.Size,
		Status:     status,
	}
	valid, err := service.cached(Object{Digest: locked.Digest, Size: locked.Size})
	if err != nil {
		if !corrupted(err) {
			return Asset{}, err
		}
		view.Corrupt = true
		return view, nil
	}
	view.Cached = valid
	if valid {
		view.Path, err = service.objectPath(locked.Digest)
		if err != nil {
			return Asset{}, err
		}
	}
	return view, nil
}
