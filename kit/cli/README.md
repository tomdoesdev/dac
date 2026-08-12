# cli

`cli` is a small command router for Go programs that want generated help and
typed struct binding without handing process control to a framework. `App.Run`
returns errors; it never prints failures or calls `os.Exit`.

```go
type cloneCommand struct {
    Repository string        `arg:"repository" help:"repository URL"`
    Depth      int           `flag:"depth,d" help:"history depth"`
    Timeout    time.Duration `flag:"timeout,t" help:"request timeout"`
}

func (*cloneCommand) Description() string { return "Clone a repository" }
func (command *cloneCommand) Run(ctx context.Context) error { /* ... */ return nil }

app := cli.New(ctx,
    cli.WithName("tool"),
    cli.WithVersion("1.0.0"),
    cli.WithCompletion(),
)
app.MustAddCommand("repository clone <repository>", &cloneCommand{},
    cli.WithAliases("repo clone"),
)
err := app.Run(os.Args[1:])
```

## Semantic color

Generated help and warnings use semantic color automatically when their output
writer is a capable terminal. Redirected output stays plain, and the standard
`NO_COLOR`, `CLICOLOR`, and `CLICOLOR_FORCE` environment conventions are
honored. Use `WithColor(cli.ColorAlways)` or `WithColor(cli.ColorNever)` when an
application needs an explicit policy.

Command handlers can use the same fixed semantic vocabulary without depending
on the underlying terminal implementation:

```go
styler := cli.NewStyler(os.Stdout, cli.ColorAuto)
fmt.Fprintf(os.Stdout, "%s %s\n", styler.Success("created"), filename)
fmt.Fprintf(os.Stderr, "%s %s\n", styler.Warning("Warning:"), message)
```

`Styler` also provides `Heading`, `Command`, `Flag`, `Argument`, and `Error`.
It returns formatted strings rather than owning writes, so handlers retain
normal control over destinations and write-error handling.

## Patterns and binding

- Plain words form the command path: `remote add`.
- `<name>` is required, `[name]` is optional, and `[files...]` is variadic.
- `arg:"name"` binds a positional field; `flag:"verbose,v"` binds a flag.
  Use `flag:"-,o"` for a short-only flag.
- Scalar strings, booleans, integers, unsigned integers, floats, durations,
  `encoding.TextUnmarshaler`, and `pflag.Value` are supported.
- Variadic arguments are slices whose element type is one of those scalar types.
- A `[]string` flag is repeatable and appends each occurrence in command-line order.
- Initial flag values are defaults. Missing optional positionals are zeroed.

Handlers only need `Description` and `Run`. Implement `cli.HelpProvider` for
long help and `cli.Validator` for post-binding validation.

## Groups, globals, and compatibility

Use `AddGroup` to document a command-only path and `AddGlobalFlags` to bind one
shared options struct. Global flags are accepted before the command path and,
once a complete command is selected, alongside local flags. `WithAliases` keeps
renamed paths working; `WithDeprecated` adds help metadata and a warning.

Completion is opt-in. After enabling `WithCompletion`, users can run
`tool completion bash`, `tool completion zsh`, or `tool completion fish`.

## Errors and exit codes

Executables should print explicitly requested help to stdout and handle returned
errors at the process boundary. A conventional mapping is help/version success
to `0`, runtime errors to `1`, and `*cli.UsageError` to `2`. Print
`UsageError.Error()` followed by `UsageError.Usage()` on stderr.
