package command

import (
	"context"

	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/output"
	"github.com/tomdoesdev/dac/internal/project"
)

type addCommand struct {
	runtime *runtime
	Name    string   `arg:"name"`
	URL     string   `arg:"url"`
	File    string   `flag:"file" help:"local filename template"`
	Set     []string `flag:"set" help:"set an artifact variable (KEY=VALUE)"`
	Pin     pinValue `flag:"pin" help:"calculate or require a SHA-256 pin"`
}

func (*addCommand) Description() string { return "Add a desired remote artifact" }
func (command *addCommand) Validate() error {
	if err := command.runtime.Output.ValidateOptions(); err != nil {
		return err
	}
	_, err := parseSets(command.Set)
	return err
}

func (command *addCommand) Run(ctx context.Context) error {
	paths, err := command.runtime.project()
	if err != nil {
		return err
	}
	return paths.WithLock(ctx, func(ctx context.Context) error {
		value, err := manifest.Load(paths.Manifest())
		if err != nil {
			return err
		}
		if _, exists := value.Files[command.Name]; exists {
			return project.NewConfigurationError(ErrAssetAlreadyExists, project.WithAsset(command.Name))
		}
		variables, err := parseSets(command.Set)
		if err != nil {
			return project.NewConfigurationError(err)
		}
		fileName := command.File
		if fileName == "" {
			fileName, err = manifest.InferFile(command.URL)
			if err != nil {
				return project.NewConfigurationError(err, project.WithAsset(command.Name))
			}
		}
		candidate := manifest.Asset{URL: command.URL, File: fileName, Variables: variables}
		if command.Pin.digest != "" {
			candidate.Pin = command.Pin.digest
		}
		value.Files[command.Name] = candidate
		resolved, err := manifest.Resolve(value)
		if err != nil {
			return err
		}
		resolvedAsset := findResolved(resolved, command.Name)
		if command.Pin.calculate {
			candidate.Pin, err = command.runtime.calculatePin(ctx, paths, resolvedAsset)
			if err != nil {
				return err
			}
			value.Files[command.Name] = candidate
		}
		if err := manifest.Write(paths.Manifest(), value); err != nil {
			return err
		}
		result := output.Result{Name: command.Name, Status: "added", File: resolvedAsset.ResolvedFile, Digest: candidate.Pin}
		return command.runtime.Output.Success("add", paths, []output.Result{result}, nil)
	})
}
