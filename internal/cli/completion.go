package cli

// This file completes the argument people actually have to remember.
//
// Command and flag names are discoverable from --help and forgiving of a wrong
// guess. A coordinate is neither: it is three parts a project chose, DAC
// refuses anything that is not one of them exactly, and the only place it is
// written down is the manifest sitting in the directory the shell is already
// standing in. So the manifest is what the shell is offered.

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

// completeCoordinates suggests coordinates for a command that takes a list,
// leaving out the ones already named.
func (runner *runner) completeCoordinates() urfave.ShellCompleteFunc {
	return runner.completeAssets(true)
}

// completeAssets writes the coordinates a command could still be given.
//
// It suggests the whole coordinate and never the bare <namespace>/<name> that
// path and info also accept. Offering both spellings would double every list to
// save deleting an @version, and the longer one is the answer to the question
// either way.
func (runner *runner) completeAssets(repeatable bool) urfave.ShellCompleteFunc {
	return func(ctx context.Context, current *urfave.Command) {
		given := current.Args().Slice()
		last := ""
		if len(given) > 0 {
			last = given[len(given)-1]
		}
		// Every generated script sends the words already finished and adds the
		// one under the cursor back only when it starts with a dash. So a dash
		// arriving here is somebody part-way through an option, and the flags
		// are what they are asking about.
		if strings.HasPrefix(last, "-") {
			urfave.DefaultCompleteWithFlags(ctx, current)
			return
		}
		// A command that takes one coordinate already has it. Suggesting a
		// second would offer an argument the command refuses.
		if len(given) > 0 && !repeatable {
			return
		}
		manifestPath, _ := projectPaths(current)
		manifest, err := project.ReadManifest(manifestPath)
		if err != nil {
			// Completion runs wherever somebody presses tab, which is usually
			// not a project. A manifest that is missing or that DAC will not
			// read is nothing to report here: the shell falls back to its own
			// completion, where an error message would arrive in the middle of
			// a half-typed command line.
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
			// The generated scripts split each line at its first colon: what is
			// before it is the word the shell inserts, and the rest is the
			// description it lists. A coordinate holds no colon and a URL holds
			// several, so every one of them lands on the right side.
			_, _ = fmt.Fprintf(runner.stdout, "%s:%s\n", text, manifest.Assets[name].URL)
		}
	}
}
