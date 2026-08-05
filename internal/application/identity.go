package application

import (
	"fmt"
	"strings"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
)

// unknownCoordinate reports a missing coordinate and the available versions.
func unknownCoordinate[V any](name coord.Coordinate, assets map[coord.Coordinate]V) error {
	versions := coord.Versions(coord.InGroup(assets, name.Group()))
	if len(versions) == 0 {
		return &fault.Error{
			Code:    "asset_unknown",
			Message: "The project does not have this asset.",
			Details: map[string]any{"asset": name.String()},
		}
	}
	return &fault.Error{
		Code:    "asset_unknown",
		Message: "The project does not have this asset version.",
		Details: map[string]any{"asset": name.String(), "versions": versions},
		Cause:   fmt.Errorf("%s has %s", name.Group(), strings.Join(versions, ", ")),
	}
}

// sharedSources returns sibling versions that use the same source URL.
func sharedSources(manifest project.Manifest, name coord.Coordinate, url string) []string {
	shared := make([]string, 0, 2)
	for _, other := range coord.InGroup(manifest.Assets, name.Group()) {
		if other != name && manifest.Assets[other].URL == url {
			shared = append(shared, other.Version)
		}
	}
	return shared
}
