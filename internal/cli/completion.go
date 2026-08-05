package cli

// This file completes the argument people actually have to remember.
// Command and flag names are discoverable from --help and forgiving of a wrong guess.

import (
	"context"
	"fmt"
	"strings"

	urfave "github.com/urfave/cli/v3"

	"github.com/tomdoesdev/dac/internal/project"
)

// completeCoordinate suggests coordinates for a command that takes one asset.
func (runner *runner) completeCoordinate() urfave.ShellCompleteFunc {
	return runner.completeAssets(false)
}

// completeCoordinates suggests coordinates for a command that takes a list, leaving out the ones already named.
func (runner *runner) completeCoordinates() urfave.ShellCompleteFunc {
	return runner.completeAssets(true)
}

// completeHosts suggests the trusted hosts, leaving out the ones already named.
// Withdrawing trust is the one trust command whose argument has to match something
// that already exists, so it is the one worth completing.
func (runner *runner) completeHosts() urfave.ShellCompleteFunc {
	return func(ctx context.Context, current *urfave.Command) {
		given := current.Args().Slice()
		if len(given) > 0 && strings.HasPrefix(given[len(given)-1], "-") {
			urfave.DefaultCompleteWithFlags(ctx, current)
			return
		}
		_, list, err := runner.trustFile(current)
		if err != nil {
			// Completion runs wherever somebody presses tab, and a file it cannot read is not the moment to say so.
			return
		}
		named := make(map[string]struct{}, len(given))
		for _, value := range given {
			named[value] = struct{}{}
		}
		for _, host := range list.Sorted() {
			if _, taken := named[host]; taken {
				continue
			}
			_, _ = fmt.Fprintf(runner.stdout, "%s:%s\n", host, usedText(list.Hosts[host].LastUsed))
		}
	}
}

// completeAssets writes the coordinates a command could still be given.
// It suggests the whole coordinate and never the bare <namespace>/<name> that path and info also accept.
func (runner *runner) completeAssets(repeatable bool) urfave.ShellCompleteFunc {
	return func(ctx context.Context, current *urfave.Command) {
		given := current.Args().Slice()
		last := ""
		if len(given) > 0 {
			last = given[len(given)-1]
		}
		// Every generated script sends the words already finished and adds the one under the cursor back only when it starts with a dash.
		if strings.HasPrefix(last, "-") {
			urfave.DefaultCompleteWithFlags(ctx, current)
			return
		}
		// A command that takes one coordinate already has it.
		if len(given) > 0 && !repeatable {
			return
		}
		manifestPath, _ := projectPaths(current)
		manifest, err := project.ReadManifest(manifestPath)
		if err != nil {
			// Completion runs wherever somebody presses tab, which is usually not a project.
			return
		}
		named := make(map[string]struct{}, len(given))
		for _, value := range given {
			named[value] = struct{}{}
		}
		for _, name := range manifest.Coordinates() {
			text := name.String()
			if _, taken := named[text]; taken {
				continue
			}
			// The generated scripts split each line at its first colon: what is before it is the word the shell inserts, and the rest is the description it lists.
			_, _ = fmt.Fprintf(runner.stdout, "%s:%s\n", text, manifest.Assets[name].URL)
		}
	}
}
