package application

import (
	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/project"
)

// AssetSummary is the part of a command result that reports the assets it
// installed. Embedding it keeps those JSON field names identical between pull
// and pack, which is the point of a versioned output contract.
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
//
// It reports the coordinate whole and in parts. The whole is what a script
// passes back to dac path; the parts are what a script branches on, and
// splitting a coordinate correctly is DAC's job rather than every consumer's.
type Asset struct {
	Coordinate string `json:"coordinate"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	URL        string `json:"url"`
	Integrity  string `json:"integrity,omitempty"`
	// Filename is what the origin calls this asset. The cache path names the
	// bytes, which is the wrong name for anything that reads an extension, so
	// this is the half a script needs to put the file somewhere useful. It is
	// absent for an asset no lock entry describes yet.
	Filename string `json:"filename,omitempty"`
	Digest   string `json:"digest"`
	Size     int64  `json:"size"`
	Cached   bool   `json:"cached"`
	// Corrupt marks an object the cache holds but cannot vouch for. It is
	// separate from Cached because the two answer different questions: whether a
	// command can use the object, and whether it should say why not.
	Corrupt bool   `json:"corrupt,omitempty"`
	Path    string `json:"path,omitempty"`
	Status  string `json:"status,omitempty"`
}

// assetView reports the state of one asset. It renders a corrupt object as a
// flag rather than an error, because every caller wants a different answer to
// it: pull downloads the object again, pack refuses, info prints it.
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
