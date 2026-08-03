package application

import "github.com/tom/dac/internal/project"

// AssetSummary is the part of a command result that reports the assets it
// installed. Embedding it keeps those JSON field names identical between pull
// and export, which is the point of a versioned output contract.
type AssetSummary struct {
	Assets     []Asset `json:"assets"`
	AssetCount int     `json:"assetCount"`
	ByteCount  int64   `json:"byteCount"`
}

// collect summarizes the assets a command touched. It never reports a null
// assets array, because a consumer should be able to range over it unchecked.
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
type Asset struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	URL       string `json:"url"`
	Integrity string `json:"integrity,omitempty"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	Cached    bool   `json:"cached"`
	Path      string `json:"path,omitempty"`
	Status    string `json:"status,omitempty"`
}

func (service *Service) assetView(name string, source project.Asset, locked project.LockAsset, status string) (Asset, error) {
	view := Asset{
		Name:      name,
		Version:   source.Version,
		URL:       source.URL,
		Integrity: source.Integrity,
		Digest:    locked.Digest,
		Size:      locked.Size,
		Status:    status,
	}
	valid, err := service.cached(Object{Digest: locked.Digest, Size: locked.Size})
	if err != nil {
		return Asset{}, err
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
