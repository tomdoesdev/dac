package application

import (
	"github.com/tom/dac/internal/coord"
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
	cacheCorrupt     = "corrupt"
	cacheUnavailable = "unavailable"
	requestAllowed   = "allowed"
	requestBlocked   = "blocked"
)

// InfoOptions selects the assets and rewrite config for one inspection.
type InfoOptions struct {
	// Selection narrows the assets to one coordinate or one asset's versions.
	// A zero Selection covers the whole project.
	Selection Selection
	Rewriter  *rewrite.Config
}

// Selection is the asset filter one inspection applies.
//
// A project can hold several versions of an asset now, so "which versions of
// this do I have" is a question with an answer, and info is the command that
// has it. The two narrower forms are the coordinate every other command takes
// and the namespace/name it belongs to.
//
// Its fields are unexported and reached only through the three constructors, so
// there is no way to build one that means nothing in particular. Its zero value
// is the whole project, which is the reading that fails safe: a filter nobody
// set should show everything rather than silently match one thing.
type Selection struct {
	kind       selectionKind
	coordinate coord.Coordinate
	group      coord.Group
}

type selectionKind int

const (
	selectEvery selectionKind = iota
	selectExact
	selectGroup
)

// EverySelection selects the whole project.
func EverySelection() Selection { return Selection{kind: selectEvery} }

// ExactSelection selects one coordinate.
func ExactSelection(name coord.Coordinate) Selection {
	return Selection{kind: selectExact, coordinate: name}
}

// GroupSelection selects every version of one asset.
func GroupSelection(group coord.Group) Selection {
	return Selection{kind: selectGroup, group: group}
}

// InfoAsset combines manifest, request, lock, and cache information.
type InfoAsset struct {
	Coordinate    string `json:"coordinate"`
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	Version       string `json:"version"`
	SourceURL     string `json:"sourceUrl"`
	RequestURL    string `json:"requestUrl"`
	RequestStatus string `json:"requestStatus"`
	Rewritten     bool   `json:"rewritten"`
	CacheStatus   string `json:"cacheStatus"`
	Integrity     string `json:"integrity,omitempty"`
	// Filename is what the origin calls this asset. It comes from the lock, so
	// it is absent for the same reason digest and size are: a missing or stale
	// lock holds nothing that describes the manifest in front of it.
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

// InfoSummary counts the states worth acting on. It deliberately omits an
// allowed count: the interesting number is how many assets a config refuses,
// and the rest is arithmetic on the asset count.
type InfoSummary struct {
	AssetCount   int    `json:"assetCount"`
	CachedCount  int    `json:"cachedCount"`
	CorruptCount int    `json:"corruptCount"`
	BlockedCount int    `json:"blockedCount"`
	LockStatus   string `json:"lockStatus"`
}

// Info inspects manifest assets without network access. It ignores lock data
// when the lock is missing or stale because that data does not describe the manifest.
func (service *Service) Info(options InfoOptions) (InfoResult, error) {
	manifest, err := service.readManifest()
	if err != nil {
		return InfoResult{}, err
	}
	names, err := selected(manifest, options.Selection)
	if err != nil {
		return InfoResult{}, err
	}
	lock, lockStatus, err := service.infoLock(manifest)
	if err != nil {
		return InfoResult{}, err
	}
	result := InfoResult{
		Assets:  make([]InfoAsset, 0, len(names)),
		Summary: InfoSummary{AssetCount: len(names), LockStatus: lockStatus},
	}
	for _, name := range names {
		asset, err := service.infoAsset(name, manifest.Assets[name], lock, lockStatus, options.Rewriter)
		if err != nil {
			return InfoResult{}, err
		}
		switch asset.CacheStatus {
		case cacheCached:
			result.Summary.CachedCount++
		case cacheCorrupt:
			result.Summary.CorruptCount++
		}
		if asset.RequestStatus == requestBlocked {
			result.Summary.BlockedCount++
		}
		result.Assets = append(result.Assets, asset)
	}
	return result, nil
}

// selected returns the coordinates one filter covers, in a stable order.
func selected(manifest project.Manifest, selection Selection) ([]coord.Coordinate, error) {
	switch selection.kind {
	case selectExact:
		if _, exists := manifest.Assets[selection.coordinate]; !exists {
			return nil, unknownCoordinate(selection.coordinate, manifest.Assets)
		}
		return []coord.Coordinate{selection.coordinate}, nil
	case selectGroup:
		names := coord.InGroup(manifest.Assets, selection.group)
		if len(names) == 0 {
			return nil, &fault.Error{
				Code:    "asset_unknown",
				Message: "The project does not have this asset.",
				Details: map[string]any{"asset": selection.group.String()},
			}
		}
		return names, nil
	default:
		return manifest.Coordinates(), nil
	}
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
func (service *Service) infoAsset(name coord.Coordinate, source project.Asset, lock project.Lock, lockStatus string, config *rewrite.Config) (InfoAsset, error) {
	decision, err := config.Evaluate(source.URL)
	if err != nil {
		return InfoAsset{}, withAsset(fault.Wrap("rewrite_failed", "DAC could not apply the rewrite config.", err), name.String())
	}
	requestStatus := requestAllowed
	if decision.Blocked {
		requestStatus = requestBlocked
	}
	result := InfoAsset{
		Coordinate:    name.String(),
		Namespace:     name.Namespace,
		Name:          name.Name,
		Version:       name.Version,
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
		return InfoAsset{}, withAsset(err, name.String())
	}
	result.Filename = view.Filename
	result.Digest = view.Digest
	result.Size = &view.Size
	switch {
	case view.Corrupt:
		result.CacheStatus = cacheCorrupt
	case view.Cached:
		result.CacheStatus = cacheCached
		result.Path = view.Path
	default:
		result.CacheStatus = cacheMissing
	}
	return result, nil
}
