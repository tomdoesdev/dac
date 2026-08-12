package command

import (
	"context"
	"errors"

	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/output"
	"github.com/tomdoesdev/dac/internal/project"
)

type updateCommand struct {
	runtime *runtime
	Name    string   `arg:"name"`
	Set     []string `flag:"set" help:"set an artifact variable (KEY=VALUE)"`
	Pin     pinValue `flag:"pin" help:"calculate or require a SHA-256 pin"`
	Unpin   bool     `flag:"unpin" help:"remove the artifact pin"`
}

func (*updateCommand) Description() string { return "Update a desired remote artifact" }
func (command *updateCommand) Validate() error {
	if err := command.runtime.Output.ValidateOptions(); err != nil {
		return err
	}
	if command.Pin.set && command.Unpin {
		return errors.New("--pin and --unpin cannot be combined")
	}
	_, err := parseSets(command.Set)
	return err
}

func (command *updateCommand) Run(ctx context.Context) error {
	paths, err := command.runtime.project()
	if err != nil {
		return err
	}
	return paths.WithLock(ctx, func(ctx context.Context) error {
		value, err := manifest.Load(paths.Manifest())
		if err != nil {
			return err
		}
		file, exists := value.Files[command.Name]
		if !exists {
			return project.NewConfigurationError(errors.New("asset does not exist"), project.WithAsset(command.Name))
		}
		sets, err := parseSets(command.Set)
		if err != nil {
			return project.NewConfigurationError(err)
		}
		if file.Variables == nil {
			file.Variables = map[string]string{}
		}
		for key, value := range sets {
			file.Variables[key] = value
		}
		if command.Unpin {
			file.Pin = ""
		}
		if command.Pin.digest != "" {
			file.Pin = command.Pin.digest
		}
		value.Files[command.Name] = file
		resolved, err := manifest.Resolve(value)
		if err != nil {
			return err
		}
		resolvedAsset := findResolved(resolved, command.Name)
		if command.Pin.calculate {
			file.Pin, err = command.runtime.calculatePin(ctx, paths, resolvedAsset)
			if err != nil {
				return err
			}
			value.Files[command.Name] = file
		}
		if err := manifest.Write(paths.Manifest(), value); err != nil {
			return err
		}
		result := output.Result{Name: command.Name, Status: "updated", File: resolvedAsset.ResolvedFile, Digest: file.Pin}
		return command.runtime.Output.Success("update", paths, []output.Result{result}, nil)
	})
}
