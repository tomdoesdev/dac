package cli

import (
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/dac"
	"github.com/tomdoesdev/dac/internal/fault"
)

// coordinate reads the one complete coordinate a command acts on.
// A command that writes takes the whole coordinate, including the version.
func coordinate(current *urfave.Command) (coord.Coordinate, error) {
	values := current.Args().Slice()
	if len(values) != 1 {
		return coord.Coordinate{}, fault.New("invalid_arguments", "Specify one asset coordinate as <namespace>/<name>@<version>.")
	}
	return parseCoordinate(values[0])
}

// coordinates reads the one or more complete coordinates a command acts on.
// coordinates validates every requested asset before cache removal starts.
func coordinates(current *urfave.Command) ([]coord.Coordinate, error) {
	values := current.Args().Slice()
	if len(values) == 0 {
		return nil, fault.New("invalid_arguments", "Specify at least one asset coordinate as <namespace>/<name>@<version>.")
	}
	names := make([]coord.Coordinate, 0, len(values))
	for _, value := range values {
		name, err := parseCoordinate(value)
		if err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, nil
}

// coordinateAndSource reads the coordinate and the source URL that add takes.
// The required URL is positional because it forms the add with the coordinate.
func coordinateAndSource(current *urfave.Command) (coord.Coordinate, string, error) {
	values := current.Args().Slice()
	if len(values) != 2 {
		return coord.Coordinate{}, "", fault.New("invalid_arguments",
			"Specify the asset coordinate and its source URL as <namespace>/<name>@<version> <url>.")
	}
	name, err := parseCoordinate(values[0])
	if err != nil {
		return coord.Coordinate{}, "", err
	}
	source := strings.TrimSpace(values[1])
	if source == "" {
		return coord.Coordinate{}, "", fault.New("invalid_arguments", "The source URL is empty.")
	}
	return name, source, nil
}

func parseCoordinate(value string) (coord.Coordinate, error) {
	name, err := coord.Parse(value)
	if err != nil {
		return coord.Coordinate{}, fault.Wrap("invalid_arguments", "The asset coordinate is invalid.", err)
	}
	return name, nil
}

// selection reads the filter info applies: nothing, one coordinate, or one asset whose versions it should list.
// Info is the command whose job is to answer what a project has, so leaving the version off is a question with a list for an answer.
func selection(current *urfave.Command) (dac.Selection, error) {
	values := current.Args().Slice()
	switch {
	case len(values) == 0:
		return dac.EverySelection(), nil
	case len(values) > 1:
		return dac.Selection{}, fault.New("invalid_arguments", "Specify at most one asset as <namespace>/<name> or <namespace>/<name>@<version>.")
	}
	return parseAsset(values[0])
}

// selections reads the assets a command was narrowed to, which may be none.
// selections gives pull and unpack the same exact and group grammar.
func selections(current *urfave.Command) ([]dac.Selection, error) {
	values := current.Args().Slice()
	chosen := make([]dac.Selection, 0, len(values))
	for _, value := range values {
		selection, err := parseAsset(value)
		if err != nil {
			return nil, err
		}
		chosen = append(chosen, selection)
	}
	return chosen, nil
}

// parseAsset reads one asset argument in either of the two spellings.
func parseAsset(value string) (dac.Selection, error) {
	if strings.Contains(value, "@") {
		name, err := parseCoordinate(value)
		if err != nil {
			return dac.Selection{}, err
		}
		return dac.ExactSelection(name), nil
	}
	group, err := coord.ParseGroup(value)
	if err != nil {
		return dac.Selection{}, fault.Wrap("invalid_arguments", "The asset is invalid.", err)
	}
	return dac.GroupSelection(group), nil
}

// destination reads the directory an unpack writes into.
// destination rejects an empty flag so an unset variable cannot select the work directory.
func destination(current *urfave.Command) (string, error) {
	trimmed := strings.TrimSpace(current.String("dest"))
	if trimmed == "" {
		return "", fault.New("invalid_arguments", "The destination directory is empty.")
	}
	return trimmed, nil
}

func noArguments(current *urfave.Command) error {
	if current.Args().Present() {
		return fault.New("invalid_arguments", "The command has unexpected arguments.")
	}
	return nil
}
