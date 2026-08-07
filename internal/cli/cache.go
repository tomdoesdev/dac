package cli

import urfave "github.com/urfave/cli/v3"

func (runner *runner) cacheCommand() *urfave.Command {
	return &urfave.Command{
		Name:            "cache",
		Usage:           "Inspect, check, and collect the object cache.",
		HideHelpCommand: true,
		Commands: []*urfave.Command{
			runner.cacheDirCommand(),
			runner.cacheListCommand(),
			runner.cacheGCCommand(),
			runner.cacheClearCommand(),
			runner.cacheRemoveCommand(),
			runner.cacheScrubCommand(),
		},
		Action: runner.helpOrInvalid,
	}
}
