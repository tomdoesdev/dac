package application

import (
	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/fault"
)

// Selection identifies all assets, one coordinate, or all versions in a group.
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

// EverySelection selects all project assets.
func EverySelection() Selection { return Selection{kind: selectEvery} }

// ExactSelection selects one coordinate.
func ExactSelection(name coord.Coordinate) Selection {
	return Selection{kind: selectExact, coordinate: name}
}

// GroupSelection selects all versions in one asset group.
func GroupSelection(group coord.Group) Selection {
	return Selection{kind: selectGroup, group: group}
}

// selected resolves one selection in stable project order.
func selected[V any](assets map[coord.Coordinate]V, selection Selection) ([]coord.Coordinate, error) {
	switch selection.kind {
	case selectExact:
		if _, exists := assets[selection.coordinate]; !exists {
			return nil, unknownCoordinate(selection.coordinate, assets)
		}
		return []coord.Coordinate{selection.coordinate}, nil
	case selectGroup:
		names := coord.InGroup(assets, selection.group)
		if len(names) == 0 {
			return nil, &fault.Error{
				Code:    "asset_unknown",
				Message: "The project does not have this asset.",
				Details: map[string]any{"asset": selection.group.String()},
			}
		}
		return names, nil
	default:
		return coord.Sorted(assets), nil
	}
}

// chosen resolves a selection list without duplicate coordinates.
func chosen[V any](assets map[coord.Coordinate]V, selections []Selection) ([]coord.Coordinate, error) {
	if len(selections) == 0 {
		return coord.Sorted(assets), nil
	}
	wanted := make(map[coord.Coordinate]struct{}, len(selections))
	for _, selection := range selections {
		names, err := selected(assets, selection)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			wanted[name] = struct{}{}
		}
	}
	names := make([]coord.Coordinate, 0, len(wanted))
	for _, name := range coord.Sorted(assets) {
		if _, found := wanted[name]; found {
			names = append(names, name)
		}
	}
	return names, nil
}
