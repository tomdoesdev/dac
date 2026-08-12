package command

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/output"
	"github.com/tomdoesdev/dac/internal/project"
)

type initCommand struct{ runtime *runtime }

func (*initCommand) Description() string { return "Create a dac project in the current directory" }
func (command *initCommand) Run(ctx context.Context) error {
	directory, err := command.runtime.CWD()
	if err != nil {
		return project.NewError("filesystem", err)
	}
	paths := project.Paths{Root: directory}
	return paths.WithLock(ctx, func(context.Context) error {
		if _, err := os.Stat(paths.Manifest()); err == nil {
			return &project.Error{Kind: "configuration", Err: errors.New("dac.toml already exists")}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return project.NewError("filesystem", err)
		}
		downloads, err := paths.OpenDownloads()
		if err != nil {
			return err
		}
		_ = downloads.Close()
		value := manifest.Manifest{Version: project.Version, Files: map[string]manifest.Asset{}}
		if err := manifest.Create(paths.Manifest(), value); err != nil {
			return err
		}
		return command.runtime.Output.Success("init", paths, []output.Result{{Name: filepath.Base(paths.Root), Status: "initialized"}}, nil)
	})
}
