package cli

import (
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/application"
	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/fault"
)

// coordinate reads the one complete coordinate a command acts on.
//
// A command that writes takes the whole coordinate, including the version. A
// project can hold several versions of an asset, and a command that changed one
// it picked for itself would do the wrong thing on the day somebody added the
// second version, which is the worst moment for it to start guessing. Reading
// commands take the shorter form through asset below, where the guess can be
// refused instead.
func coordinate(current *urfave.Command) (coord.Coordinate, error) {
	values := current.Args().Slice()
	if len(values) != 1 {
		return coord.Coordinate{}, fault.New("invalid_arguments", "Specify one asset coordinate as <namespace>/<name>@<version>.")
	}
	return parseCoordinate(values[0])
}

// coordinates reads the one or more complete coordinates a command acts on.
//
// It takes several because forgetting cached bytes is the kind of thing done to
// a handful of assets at once, and because each one is checked against the
// project before anything is removed -- so a list with a typo in it removes
// nothing rather than most of what was asked for.
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
//
// The URL is an argument rather than a flag because it is not optional and it
// is not a setting: an add is "this coordinate comes from here", and both
// halves of that are the thing being said. A required flag is a flag in name
// only, and writing it out on every add bought nothing.
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

// selection reads the filter info applies: nothing, one coordinate, or one
// asset whose versions it should list.
//
// Info is the command whose job is to answer what a project has, so leaving the
// version off is a question with a list for an answer.
func selection(current *urfave.Command) (application.Selection, error) {
	values := current.Args().Slice()
	switch {
	case len(values) == 0:
		return application.EverySelection(), nil
	case len(values) > 1:
		return application.Selection{}, fault.New("invalid_arguments", "Specify at most one asset as <namespace>/<name> or <namespace>/<name>@<version>.")
	}
	return parseAsset(values[0])
}

// selections reads the assets a command was narrowed to, which may be none.
//
// It takes the same two spellings info does, because narrowing a command and
// asking about a project are the same question with different verbs: a
// coordinate names one version, and a bare <namespace>/<name> names every
// version of that asset. No arguments at all is the whole project, which is
// what these commands did before they could be narrowed.
func selections(current *urfave.Command) ([]application.Selection, error) {
	values := current.Args().Slice()
	chosen := make([]application.Selection, 0, len(values))
	for _, value := range values {
		selection, err := parseAsset(value)
		if err != nil {
			return nil, err
		}
		chosen = append(chosen, selection)
	}
	return chosen, nil
}

// asset reads the one asset a command acts on, with or without its version.
//
// Leaving the version off asks DAC to work out which one was meant, and it will
// only do that when the project leaves nothing to work out. See onlyAsset in
// the application package for what happens when it does not.
func asset(current *urfave.Command) (application.Selection, error) {
	values := current.Args().Slice()
	if len(values) != 1 {
		return application.Selection{}, fault.New("invalid_arguments", "Specify one asset as <namespace>/<name> or <namespace>/<name>@<version>.")
	}
	return parseAsset(values[0])
}

// parseAsset reads one asset argument in either of the two spellings.
func parseAsset(value string) (application.Selection, error) {
	if strings.Contains(value, "@") {
		name, err := parseCoordinate(value)
		if err != nil {
			return application.Selection{}, err
		}
		return application.ExactSelection(name), nil
	}
	group, err := coord.ParseGroup(value)
	if err != nil {
		return application.Selection{}, fault.Wrap("invalid_arguments", "The asset is invalid.", err)
	}
	return application.GroupSelection(group), nil
}

// optionalPaths reads up to most positional paths, each of which has a default
// the caller supplies when it is left off.
//
// A path is trimmed and refused when empty, because an empty argument is a
// shell variable nobody set rather than a request for the current directory,
// and the two would otherwise be the same command.
func optionalPaths(current *urfave.Command, most int, usage string) ([]string, error) {
	values := current.Args().Slice()
	if len(values) > most {
		return nil, fault.New("invalid_arguments", usage)
	}
	paths := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return nil, fault.New("invalid_arguments", usage)
		}
		paths = append(paths, trimmed)
	}
	return paths, nil
}

// requiredPath reads the one positional path a command has no default for.
//
// It is optionalPaths with the fallback taken away: same trimming, same refusal
// of an empty argument, but leaving it off is an error rather than a default. A
// cache bundle is a thing being moved somewhere and the somewhere is the point
// of the command, so there is no file DAC could pick unasked -- which is a
// reason to require the argument, not a reason to spell it as a flag.
func requiredPath(current *urfave.Command, usage string) (string, error) {
	values := current.Args().Slice()
	if len(values) != 1 {
		return "", fault.New("invalid_arguments", usage)
	}
	trimmed := strings.TrimSpace(values[0])
	if trimmed == "" {
		return "", fault.New("invalid_arguments", usage)
	}
	return trimmed, nil
}

// pathOr returns the positional at position, or fallback when it was left off.
func pathOr(paths []string, position int, fallback string) string {
	if position < len(paths) {
		return paths[position]
	}
	return fallback
}

func noArguments(current *urfave.Command) error {
	if current.Args().Present() {
		return fault.New("invalid_arguments", "The command has unexpected arguments.")
	}
	return nil
}
