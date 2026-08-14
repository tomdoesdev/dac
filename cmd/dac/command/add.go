package command

import (
	"context"

	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/output"
)

type addCommand struct {
	runtime *runtime
	URLs    []string `arg:"urls"`
}

func newAddCommand(runtime *runtime) *addCommand { return &addCommand{runtime: runtime} }

func (*addCommand) Description() string { return "Add one or more desired remote artifacts" }
func (command *addCommand) Validate() error {
	return command.runtime.Output.ValidateOptions()
}

func (command *addCommand) Run(ctx context.Context) error {
	paths, err := command.runtime.project()
	if err != nil {
		return err
	}
	return paths.WithLock(ctx, func(context.Context) error {
		value, err := manifest.Load(paths.Manifest())
		if err != nil {
			return err
		}
		results := make([]output.Result, 0, len(command.URLs))
		for _, rawURL := range command.URLs {
			name, err := manifest.InferFile(rawURL)
			if err != nil {
				return fault.NewConfigurationError(err)
			}
			if _, exists := value.Files[name]; exists {
				return fault.NewConfigurationError(ErrAssetAlreadyExists, fault.WithAsset(name))
			}
			value.Files[name] = manifest.Asset{URL: rawURL, File: name}
			results = append(results, output.Result{Name: name, Status: "added", File: name})
		}
		// Available values are resolved before the single manifest write. Missing
		// references are intentional setup state; every other error aborts the batch.
		if err := manifest.ValidateAvailable(value); err != nil {
			return err
		}
		if err := manifest.Write(paths.Manifest(), value); err != nil {
			return err
		}
		return command.runtime.Output.Success("add", paths.Root, results, nil)
	})
}
