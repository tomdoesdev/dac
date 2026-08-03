package cli

import (
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tom/dac/internal/application"
	"github.com/tom/dac/internal/coord"
	"github.com/tom/dac/internal/fault"
)

// coordinate reads the one complete coordinate a command acts on.
//
// Every command that changes or reads one asset takes the whole coordinate,
// including the version. A project can hold several versions of an asset, and a
// command that accepted a bare name would work until the day somebody added the
// second version, which is the worst moment for it to start guessing.
func coordinate(current *urfave.Command) (coord.Coordinate, error) {
	values := current.Args().Slice()
	if len(values) != 1 {
		return coord.Coordinate{}, fault.New("invalid_arguments", "Specify one asset coordinate as <namespace>/<name>@<version>.")
	}
	value, err := coord.Parse(values[0])
	if err != nil {
		return coord.Coordinate{}, fault.Wrap("invalid_arguments", "The asset coordinate is invalid.", err)
	}
	return value, nil
}

// selection reads the filter info applies: nothing, one coordinate, or one
// asset whose versions it should list.
//
// Info is the one command that takes the shorter form, because it is the one
// command whose job is to answer what a project has. Reading an asset is
// unambiguous, and asking which versions exist is a question with a list for an
// answer rather than a guess.
func selection(current *urfave.Command) (application.Selection, error) {
	values := current.Args().Slice()
	switch {
	case len(values) == 0:
		return application.EverySelection(), nil
	case len(values) > 1:
		return application.Selection{}, fault.New("invalid_arguments", "Specify at most one asset as <namespace>/<name> or <namespace>/<name>@<version>.")
	}
	if strings.Contains(values[0], "@") {
		value, err := coord.Parse(values[0])
		if err != nil {
			return application.Selection{}, fault.Wrap("invalid_arguments", "The asset coordinate is invalid.", err)
		}
		return application.ExactSelection(value), nil
	}
	group, err := coord.ParseGroup(values[0])
	if err != nil {
		return application.Selection{}, fault.Wrap("invalid_arguments", "The asset is invalid.", err)
	}
	return application.GroupSelection(group), nil
}

func noArguments(current *urfave.Command) error {
	if current.Args().Present() {
		return fault.New("invalid_arguments", "The command has unexpected arguments.")
	}
	return nil
}
