// Package cli builds small, testable command-line applications from command
// patterns and tagged handler structs.
//
// Command patterns contain plain command literals followed by required
// (<name>) or optional ([name]) positional arguments. A final positional may be
// variadic by adding an ellipsis, as in [files...]. Fields tagged with arg bind
// those positionals; fields tagged with flag bind GNU-style long and short flags.
//
// App.Run never terminates the process. It returns UsageError for invocation and
// validation failures so the executable can choose presentation and exit codes.
package cli
