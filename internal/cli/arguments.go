package cli

import (
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tom/dac/internal/fault"
)

func coordinate(current *urfave.Command) (string, string, error) {
	values := current.Args().Slice()
	if len(values) != 1 || strings.Count(values[0], "@") != 1 {
		return "", "", fault.New("invalid_arguments", "Specify one asset coordinate as name@version.")
	}
	name, version, _ := strings.Cut(values[0], "@")
	if name == "" || version == "" {
		return "", "", fault.New("invalid_arguments", "The asset name and version must not be empty.")
	}
	return name, version, nil
}

// optionalCoordinate accepts no coordinate or one exact coordinate.
func optionalCoordinate(current *urfave.Command) (string, string, error) {
	if !current.Args().Present() {
		return "", "", nil
	}
	return coordinate(current)
}

func noArguments(current *urfave.Command) error {
	if current.Args().Present() {
		return fault.New("invalid_arguments", "The command has unexpected arguments.")
	}
	return nil
}
