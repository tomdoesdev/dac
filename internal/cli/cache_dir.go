package cli

import (
	"context"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/dac"
)

func (runner *runner) cacheDirCommand() *urfave.Command {
	return &urfave.Command{
		Name:  "dir",
		Usage: "Print the resolved cache directory.",
		Action: runner.runBare("cache.dir", func(_ context.Context, current *urfave.Command) (any, string, error) {
			root, err := runner.cacheRoot(current)
			if err != nil {
				return nil, "", err
			}
			return dac.CacheDirResult{Path: root}, root, nil
		}),
	}
}
