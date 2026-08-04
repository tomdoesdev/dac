package cli

import (
	"context"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tom/dac/internal/application"
)

// configCommand builds the configuration inspection commands.
//
// dac cache dir exists because "where does DAC put things" is a real question
// that used to have no answer but a path printed beside a cached asset. A
// config file merged across the XDG search path creates the same question twice
// over -- which files was it, and what did they settle on -- so it gets the
// same treatment rather than leaving people to guess at a search path.
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
		Action: runner.run("config.path", func(_ context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
				return nil, "", err
			}
			settings, err := runner.config(current)
			if err != nil {
				return nil, "", err
			}
			// One path per line and nothing else, so a script can read it the
			// way it reads dac cache dir. A machine with no config file prints
			// nothing at all rather than a sentence saying so.
			return application.ConfigPathResult{Files: settings.Files}, strings.Join(settings.Files, "\n"), nil
		}),
	}
}

// configShowCommand reports the effective settings and where each came from.
func (runner *runner) configShowCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "show",
		Usage: "Print the effective configuration as a config file.",
		Action: runner.run("config.show", func(_ context.Context, current *urfave.Command) (any, string, error) {
			if err := noArguments(current); err != nil {
				return nil, "", err
			}
			settings, err := runner.config(current)
			if err != nil {
				return nil, "", err
			}
			result := application.ConfigShowResult{Files: settings.Files}
			for _, setting := range settings.Settings() {
				result.Settings = append(result.Settings, application.ConfigSetting{
					Key:    setting.Key,
					Value:  setting.Value,
					Source: setting.Source,
				})
			}
			// The human form is a config file. The answer to "what is DAC
			// using" is then also the starting point for changing it.
			return result, strings.TrimRight(settings.TOML(), "\n"), nil
		}),
	}
}
