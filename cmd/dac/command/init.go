package command

import (
	"context"
	"path/filepath"

	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/output"
	"github.com/tomdoesdev/dac/internal/project"
)

type initCommand struct{ runtime *runtime }

func (*initCommand) Description() string { return "Create a dac project in the current directory" }
func (command *initCommand) Validate() error {
	return command.runtime.Output.ValidateOptions()
}
func (command *initCommand) Run(ctx context.Context) error {
	directory, err := command.runtime.CWD()
	if err != nil {
		return fault.NewFilesystemError(err)
	}
	paths := project.Paths{Root: directory}
	return paths.WithLock(ctx, func(context.Context) error {
		// Create is the single existence check: its no-replace commit is already
		// race-free, so a separate stat would only duplicate the rule. It runs
		// first so initializing an existing project changes nothing on disk.
		value := manifest.Manifest{Version: manifest.Version, Files: map[string]manifest.Asset{}}
		if err := manifest.Create(paths.Manifest(), value); err != nil {
			return err
		}
		downloads, err := paths.OpenDownloads()
		if err != nil {
			return err
		}
		_ = downloads.Close()
		return command.runtime.Output.Success("init", paths.Root, []output.Result{{Name: filepath.Base(paths.Root), Status: "initialized"}}, nil)
	})
}
