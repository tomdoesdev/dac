package dac

import (
	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
)

// RemoveResult reports the asset removed from a project.
type RemoveResult struct {
	Coordinate string `json:"coordinate"`
	Namespace  string `json:"namespace"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	AssetCount int    `json:"assetCount"`
	// Remaining names the versions of this asset the project still has.
	Remaining []string `json:"remaining"`
	// Unlocked remains for result compatibility. Remove leaves the complete lock file untouched.
	Unlocked []string `json:"unlocked"`
}

// Remove deletes one exact coordinate from the manifest without reading or writing the lock file.
// This keeps the command usable offline even when the lock is missing, stale, or invalid.
func (service *Service) Remove(name coord.Coordinate) (RemoveResult, error) {
	manifest, err := service.readManifest()
	if err != nil {
		return RemoveResult{}, err
	}
	if _, exists := manifest.Assets[name]; !exists {
		return RemoveResult{}, unknownCoordinate(name, manifest.Assets)
	}
	updated := manifest.Clone()
	delete(updated.Assets, name)
	if err := project.Write(service.ManifestPath, updated); err != nil {
		return RemoveResult{}, fault.Wrap("project_write_failed", "DAC could not write the manifest file.", err)
	}
	return RemoveResult{
		Coordinate: name.String(),
		Namespace:  name.Namespace,
		Name:       name.Name,
		Version:    name.Version,
		AssetCount: len(updated.Assets),
		Remaining:  coord.Versions(coord.InGroup(updated.Assets, name.Group())),
		Unlocked:   []string{},
	}, nil
}
