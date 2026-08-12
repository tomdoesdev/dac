package cli_test

import (
	"context"
	"fmt"
	"os"

	"github.com/tomdoesdev/kit/cli"
)

type greetingCommand struct {
	Name     string `arg:"name" help:"person to greet"`
	Enthused bool   `flag:"enthused,e" help:"add emphasis"`
}

func (*greetingCommand) Description() string { return "Greet one person" }

func (command *greetingCommand) Run(context.Context) error {
	punctuation := "."
	if command.Enthused {
		punctuation = "!"
	}
	fmt.Printf("Hello, %s%s\n", command.Name, punctuation)
	return nil
}

// Example demonstrates the minimum public workflow: define a handler with tagged
// fields, register its command pattern, and run it. The expected output also
// verifies that flags may follow positional arguments.
func Example() {
	app := cli.New(context.Background(), cli.WithName("greeter"), cli.WithOutput(os.Stdout))
	app.MustAddCommand("greet <name>", &greetingCommand{})
	if err := app.Run([]string{"greet", "Codex", "--enthused"}); err != nil {
		panic(err)
	}

	// Output:
	// Hello, Codex!
}
