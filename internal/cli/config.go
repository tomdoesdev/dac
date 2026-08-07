package cli

import (
	"context"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/config"
)

// configPathResult reports the config files one run read, most important first.
type configPathResult struct {
	Files []string `json:"files"`
}

// configShowResult reports the effective configuration.
type configShowResult struct {
	Files    []string         `json:"files"`
	Settings []config.Setting `json:"settings"`
}

// configCommand builds the configuration inspection commands.
func (runner *runner) configCommand() *urfave.Command {
	return &urfave.Command{
		Name:            "config",
		Usage:           "Show the configuration DAC is using.",
		HideHelpCommand: true,
		Commands: []*urfave.Command{
			runner.configPathCommand(),
			runner.configShowCommand(),
		},
		Action: runner.helpOrInvalid,
	}
}

// configPathCommand reports the files that were read, most important first.
func (runner *runner) configPathCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "path",
		Usage: "Print the config files DAC read, most important first.",
		Action: runner.runBare("config.path", func(_ context.Context, current *urfave.Command) (any, string, error) {
			settings, err := runner.config(current)
			if err != nil {
				return nil, "", err
			}
			// One path per line and nothing else, so a script can read it the way it reads dac cache dir.
			return configPathResult{Files: settings.Files}, strings.Join(settings.Files, "\n"), nil
		}),
	}
}

// configShowCommand reports the effective settings and where each came from.
func (runner *runner) configShowCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "show",
		Usage: "Print the effective configuration as a config file.",
		Action: runner.runBare("config.show", func(_ context.Context, current *urfave.Command) (any, string, error) {
			settings, err := runner.config(current)
			if err != nil {
				return nil, "", err
			}
			result := configShowResult{Files: settings.Files, Settings: settings.Settings()}
			// The human form is a config file.
			return result, strings.TrimRight(settings.TOML(), "\n"), nil
		}),
	}
}
