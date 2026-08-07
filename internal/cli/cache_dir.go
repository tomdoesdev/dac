package cli

import (
	"context"

	urfave "github.com/urfave/cli/v3"
)

// cacheDirResult reports where the object cache resolved to.
type cacheDirResult struct {
	Path string `json:"path"`
}

func (runner *runner) cacheDirCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "dir",
		Usage: "Print the resolved cache directory.",
		Action: runner.runBare("cache.dir", func(_ context.Context, current *urfave.Command) (any, string, error) {
			root, err := runner.cacheRoot(current)
			if err != nil {
				return nil, "", err
			}
			return cacheDirResult{Path: root}, root, nil
		}),
	}
}
