package cli

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

type minimalHandler struct {
	ran bool
}

func (*minimalHandler) Description() string { return "Run the minimal command" }

func (handler *minimalHandler) Run(context.Context) error {
	handler.ran = true
	return nil
}

type upperText string

func (value *upperText) UnmarshalText(input []byte) error {
	*value = upperText(strings.ToUpper(string(input)))
	return nil
}

type typedArgumentHandler struct {
	Count   int           `arg:"count"`
	Timeout time.Duration `arg:"timeout"`
	Mode    upperText     `arg:"mode"`
	IDs     []int         `arg:"ids"`
}

func (*typedArgumentHandler) Description() string       { return "Bind typed arguments" }
func (*typedArgumentHandler) Run(context.Context) error { return nil }

// TestMinimalHandlerAndTypedArguments covers both the smallest supported
// handler and the reflection-based conversions DAC commands rely on. It guards
// against accidentally requiring optional lifecycle methods and verifies scalar,
// duration, TextUnmarshaler, and variadic numeric argument binding together.
func TestMinimalHandlerAndTypedArguments(t *testing.T) {
	// Description and Run are sufficient; Validate and Help are intentionally
	// absent so interface growth cannot silently break minimal handlers.
	minimal := &minimalHandler{}
	minimalApp := New(context.Background(), WithOutput(&bytes.Buffer{}))
	minimalApp.MustAddCommand("minimal", minimal)
	if err := minimalApp.Run([]string{"minimal"}); err != nil {
		t.Fatalf("minimal Run() error = %v", err)
	}
	if !minimal.ran {
		t.Fatal("minimal handler did not run")
	}

	// Typed positional conversion should match flag conversion, including custom
	// text unmarshalling and element-by-element conversion of variadic slices.
	typed := &typedArgumentHandler{}
	typedApp := New(context.Background(), WithOutput(&bytes.Buffer{}))
	typedApp.MustAddCommand("typed <count> <timeout> <mode> [ids...]", typed)
	if err := typedApp.Run([]string{"typed", "3", "250ms", "fast", "7", "11"}); err != nil {
		t.Fatalf("typed Run() error = %v", err)
	}
	if typed.Count != 3 || typed.Timeout != 250*time.Millisecond || typed.Mode != "FAST" {
		t.Fatalf("typed scalar arguments = %#v", typed)
	}
	if !slices.Equal(typed.IDs, []int{7, 11}) {
		t.Fatalf("typed variadic arguments = %v", typed.IDs)
	}
}

// TestTypedArgumentErrorsNameTheArgument keeps parse failures actionable. Both
// missing and malformed positionals must identify the pattern name rather than
// exposing a reflection type or an unhelpful token index.
func TestTypedArgumentErrorsNameTheArgument(t *testing.T) {
	// Missing input should point to the first required argument.
	missingApp := New(context.Background(), WithOutput(&bytes.Buffer{}))
	missingApp.MustAddCommand("typed <count> <timeout> <mode> [ids...]", &typedArgumentHandler{})
	err := missingApp.Run([]string{"typed"})
	if err == nil || !strings.Contains(err.Error(), "<count>") {
		t.Fatalf("missing argument error = %v, want named count argument", err)
	}

	// Conversion errors should retain the same human-facing argument name.
	invalidApp := New(context.Background(), WithOutput(&bytes.Buffer{}))
	invalidApp.MustAddCommand("typed <count> <timeout> <mode> [ids...]", &typedArgumentHandler{})
	err = invalidApp.Run([]string{"typed", "not-an-int", "1s", "fast"})
	if err == nil || !strings.Contains(err.Error(), "invalid value for <count>") {
		t.Fatalf("invalid argument error = %v, want named count argument", err)
	}
}

type globalOptions struct {
	Verbose bool   `flag:"verbose,v" help:"write detailed output"`
	Config  string `flag:"config,c" help:"configuration file"`
}

type globalCommandHandler struct {
	Name  string `arg:"name"`
	Force bool   `flag:"force,f"`
}

func (*globalCommandHandler) Description() string       { return "Run with global options" }
func (*globalCommandHandler) Run(context.Context) error { return nil }

// TestGlobalFlagsBeforeAndAfterCommand pins flexible global-option placement.
// Users may put globals before the command path or alongside leaf arguments,
// while local flags and positionals must still bind to the correct handler.
func TestGlobalFlagsBeforeAndAfterCommand(t *testing.T) {
	// Leading globals exercise command discovery after consuming root options.
	beforeGlobals := &globalOptions{}
	beforeHandler := &globalCommandHandler{}
	beforeApp := New(context.Background(), WithOutput(&bytes.Buffer{}))
	beforeApp.MustAddGlobalFlags(beforeGlobals)
	beforeApp.MustAddCommand("run <name>", beforeHandler)
	if err := beforeApp.Run([]string{"-v", "--config", "settings.json", "run", "repo", "-f"}); err != nil {
		t.Fatalf("leading global flags error = %v", err)
	}
	if !beforeGlobals.Verbose || beforeGlobals.Config != "settings.json" || !beforeHandler.Force || beforeHandler.Name != "repo" {
		t.Fatalf("leading globals/local binding = globals=%#v handler=%#v", beforeGlobals, beforeHandler)
	}

	// Trailing globals exercise the leaf parser's ability to delegate recognized
	// options back to the global binding set, including --name=value syntax.
	afterGlobals := &globalOptions{}
	afterHandler := &globalCommandHandler{}
	afterApp := New(context.Background(), WithOutput(&bytes.Buffer{}))
	afterApp.MustAddCommand("run <name>", afterHandler)
	afterApp.MustAddGlobalFlags(afterGlobals)
	if err := afterApp.Run([]string{"run", "repo", "--verbose", "--config=settings.json"}); err != nil {
		t.Fatalf("trailing global flags error = %v", err)
	}
	if !afterGlobals.Verbose || afterGlobals.Config != "settings.json" {
		t.Fatalf("trailing globals = %#v", afterGlobals)
	}
}

// TestGlobalFlagHelpAndConflicts makes global options discoverable without
// allowing ambiguous ownership. Help must distinguish them, and collisions are
// rejected regardless of whether globals or the command are registered first.
func TestGlobalFlagHelpAndConflicts(t *testing.T) {
	// Leaf help needs both a usage marker and a separately labeled option list so
	// users know that globals are accepted for that command.
	output := &bytes.Buffer{}
	app := New(context.Background(), WithOutput(output), WithName("tool"))
	app.MustAddGlobalFlags(&globalOptions{})
	app.MustAddCommand("run <name>", &globalCommandHandler{})
	if err := app.Run([]string{"help", "run"}); err != nil {
		t.Fatalf("help error = %v", err)
	}
	if help := output.String(); !strings.Contains(help, "[global flags]") || !strings.Contains(help, "Global options:") || !strings.Contains(help, "--verbose") {
		t.Fatalf("global help = %q", help)
	}

	// Long and short aliases share the same namespace across global and local
	// flags; registration order must not change collision detection.
	type conflictingGlobal struct {
		Force bool `flag:"force,x"`
	}
	conflictApp := New(context.Background())
	conflictApp.MustAddCommand("run <name>", &globalCommandHandler{})
	if err := conflictApp.AddGlobalFlags(&conflictingGlobal{}); err == nil {
		t.Fatal("global/local name conflict succeeded")
	}

	reverseConflictApp := New(context.Background())
	reverseConflictApp.MustAddGlobalFlags(&conflictingGlobal{})
	if err := reverseConflictApp.AddCommand("run <name>", &globalCommandHandler{}); err == nil {
		t.Fatal("local/global name conflict succeeded")
	}
}

type outputCommandHandler struct {
	Output string `flag:"output,o"`
}

func (*outputCommandHandler) Description() string       { return "Capture an output value" }
func (*outputCommandHandler) Run(context.Context) error { return nil }

// TestLocalFlagCanConsumeAGlobalLookingValue prevents eager global parsing from
// stealing a local option's value. Once --output requires the next token, that
// token is data even if it exactly matches a registered global flag.
func TestLocalFlagCanConsumeAGlobalLookingValue(t *testing.T) {
	globals := &globalOptions{}
	handler := &outputCommandHandler{}
	app := New(context.Background(), WithOutput(&bytes.Buffer{}))
	app.MustAddGlobalFlags(globals)
	app.MustAddCommand("run", handler)
	if err := app.Run([]string{"run", "--output", "--verbose"}); err != nil {
		t.Fatalf("flag-looking value error = %v", err)
	}
	if handler.Output != "--verbose" || globals.Verbose {
		t.Fatalf("flag-looking value = output=%q verbose=%t", handler.Output, globals.Verbose)
	}
}

// TestGroupsAliasesAndDeprecation covers the metadata that lets a growing CLI
// evolve without sacrificing discoverability or compatibility.
func TestGroupsAliasesAndDeprecation(t *testing.T) {
	// An explicitly documented group should replace noisy flattened descendants
	// in root help, then reveal its own narrative and children in group help.
	groupOutput := &bytes.Buffer{}
	groupApp := New(context.Background(), WithOutput(groupOutput), WithName("tool"))
	groupApp.MustAddGroup("remote", "Manage remote repositories", WithGroupHelp("Add or remove configured remotes."))
	groupApp.MustAddCommand("remote add <name>", &nameHandler{description: "Add a remote"})
	if err := groupApp.Run(nil); err != nil {
		t.Fatalf("root help error = %v", err)
	}
	rootHelp := groupOutput.String()
	if !strings.Contains(rootHelp, "remote  Manage remote repositories") || strings.Contains(rootHelp, "remote add") {
		t.Fatalf("documented group root help = %q", rootHelp)
	}
	groupDetailOutput := &bytes.Buffer{}
	groupDetailApp := New(context.Background(), WithOutput(groupDetailOutput), WithName("tool"))
	groupDetailApp.MustAddGroup("remote", "Manage remote repositories", WithGroupHelp("Add or remove configured remotes."))
	groupDetailApp.MustAddCommand("remote add <name>", &nameHandler{description: "Add a remote"})
	if err := groupDetailApp.Run([]string{"help", "remote"}); err != nil {
		t.Fatalf("group help error = %v", err)
	}
	if help := groupDetailOutput.String(); !strings.Contains(help, "Add or remove configured remotes.") || !strings.Contains(help, "add  Add a remote") {
		t.Fatalf("group detail help = %q", help)
	}

	// An alias must route to the canonical handler, bind arguments identically,
	// and emit the configured migration warning for deprecated commands.
	aliasHandler := &nameHandler{description: "Remove a remote"}
	warnings := &bytes.Buffer{}
	aliasApp := New(context.Background(), WithOutput(&bytes.Buffer{}), WithErrorOutput(warnings))
	aliasApp.MustAddCommand("remote remove <name>", aliasHandler, WithAliases("remote rm"), WithDeprecated("use remote delete"))
	if err := aliasApp.Run([]string{"remote", "rm", "origin"}); err != nil {
		t.Fatalf("alias invocation error = %v", err)
	}
	if aliasHandler.Name != "origin" || !strings.Contains(warnings.String(), "use remote delete") {
		t.Fatalf("alias/deprecation result = name=%q warning=%q", aliasHandler.Name, warnings.String())
	}

	// Canonical help advertises aliases and deprecation so users can migrate
	// without first invoking the obsolete spelling.
	helpOutput := &bytes.Buffer{}
	helpApp := New(context.Background(), WithOutput(helpOutput))
	helpApp.MustAddCommand("remote remove <name>", &nameHandler{description: "Remove a remote"}, WithAliases("remote rm"), WithDeprecated("use remote delete"))
	if err := helpApp.Run([]string{"help", "remote", "remove"}); err != nil {
		t.Fatalf("command help error = %v", err)
	}
	if help := helpOutput.String(); !strings.Contains(help, "Aliases:") || !strings.Contains(help, "remote rm") || !strings.Contains(help, "Deprecated:") {
		t.Fatalf("alias/deprecation help = %q", help)
	}
}

// TestCommandAndFlagSuggestions ensures common command and option typos produce
// a concrete correction. Both routing and leaf parsing have independent error
// paths, so each needs coverage.
func TestCommandAndFlagSuggestions(t *testing.T) {
	// A misspelled path token should suggest the closest command literal.
	commandApp := New(context.Background(), WithOutput(&bytes.Buffer{}))
	commandApp.MustAddCommand("run", &minimalHandler{})
	err := commandApp.Run([]string{"rn"})
	if err == nil || !strings.Contains(err.Error(), `did you mean "run"`) {
		t.Fatalf("command suggestion error = %v", err)
	}

	// A misspelled long option should suggest the closest flag spelling.
	flagApp := New(context.Background(), WithOutput(&bytes.Buffer{}))
	flagApp.MustAddCommand("run", &verboseCommandHandler{})
	err = flagApp.Run([]string{"run", "--verbse"})
	if err == nil || !strings.Contains(err.Error(), `did you mean "--verbose"`) {
		t.Fatalf("flag suggestion error = %v", err)
	}
}

type verboseCommandHandler struct {
	Verbose bool `flag:"verbose,v"`
}

func (*verboseCommandHandler) Description() string       { return "Run verbosely" }
func (*verboseCommandHandler) Run(context.Context) error { return nil }

// TestFlagTagGrammarRejectsUnaddressableNames prevents struct tags from creating
// options that cannot be parsed unambiguously. It covers invalid characters in
// both the long-name and one-rune short-name positions.
func TestFlagTagGrammarRejectsUnaddressableNames(t *testing.T) {
	for name, handler := range map[string]CommandHandler{
		"long":  &invalidLongHandler{},  // '=' would split --name=value syntax.
		"short": &invalidShortHandler{}, // '=' cannot be used as a short option rune.
	} {
		t.Run(name, func(t *testing.T) {
			if err := New(context.Background()).AddCommand("run", handler); err == nil {
				t.Fatal("invalid flag tag succeeded")
			}
		})
	}
}

type invalidLongHandler struct {
	Value string `flag:"bad=name"`
}

func (*invalidLongHandler) Description() string       { return "Invalid long flag" }
func (*invalidLongHandler) Run(context.Context) error { return nil }

type invalidShortHandler struct {
	Value string `flag:"value,="`
}

func (*invalidShortHandler) Description() string       { return "Invalid short flag" }
func (*invalidShortHandler) Run(context.Context) error { return nil }

// TestVersionAndCompletion covers framework-owned commands that bypass user
// handlers. It verifies stable version text, every supported shell generator,
// rejection of unsupported shells, the public completion command, and the
// hidden candidate protocol invoked by generated scripts.
func TestVersionAndCompletion(t *testing.T) {
	// --version is intentionally exact because scripts may consume the line.
	versionOutput := &bytes.Buffer{}
	versionApp := New(context.Background(), WithName("tool"), WithVersion("1.2.3"), WithOutput(versionOutput))
	if err := versionApp.Run([]string{"--version"}); err != nil {
		t.Fatalf("version error = %v", err)
	}
	if got := versionOutput.String(); got != "tool 1.2.3\n" {
		t.Fatalf("version output = %q", got)
	}

	// Every advertised shell must receive a script wired to the hidden
	// __complete command; unsupported shells fail instead of emitting junk.
	completionApp := New(context.Background(), WithName("tool"), WithCompletion(), WithOutput(&bytes.Buffer{}))
	completionApp.MustAddCommand("remote add", &minimalHandler{})
	for _, shell := range []string{
		"bash", // Bourne-style completion using complete/compgen.
		"zsh",  // Zsh-native completion functions and candidate descriptions.
		"fish", // Fish completion declarations and command substitution.
	} {
		script, err := completionApp.Completion(shell)
		if err != nil || !strings.Contains(script, "__complete") {
			t.Fatalf("%s completion = %q, %v", shell, script, err)
		}
	}
	if _, err := completionApp.Completion("powershell"); err == nil {
		t.Fatal("unsupported completion shell succeeded")
	}
	// The visible completion subcommand exposes generators through normal CLI
	// routing rather than requiring callers to use the Go API.
	commandOutput := &bytes.Buffer{}
	commandApp := New(context.Background(), WithName("tool"), WithCompletion(), WithOutput(commandOutput))
	if err := commandApp.Run([]string{"completion", "fish"}); err != nil {
		t.Fatalf("completion command error = %v", err)
	}
	if !strings.Contains(commandOutput.String(), "__complete") {
		t.Fatalf("completion command output = %q", commandOutput.String())
	}

	// The hidden protocol returns newline-delimited candidates for a partial
	// token, which generated shell scripts can consume without extra parsing.
	candidateOutput := &bytes.Buffer{}
	candidateApp := New(context.Background(), WithCompletion(), WithOutput(candidateOutput))
	candidateApp.MustAddCommand("remote add", &minimalHandler{})
	if err := candidateApp.Run([]string{"__complete", "r"}); err != nil {
		t.Fatalf("completion candidate error = %v", err)
	}
	if got := candidateOutput.String(); got != "remote\n" {
		t.Fatalf("completion candidates = %q", got)
	}
}

// TestUsageErrorStillUnwrapsSuggestedParserErrors ensures enriching a parser
// failure with a “did you mean” hint does not bypass the UsageError wrapper that
// DAC uses to select help text and its configuration-error exit code.
func TestUsageErrorStillUnwrapsSuggestedParserErrors(t *testing.T) {
	app := New(context.Background(), WithOutput(&bytes.Buffer{}))
	app.MustAddCommand("run", &verboseCommandHandler{})
	err := app.Run([]string{"run", "--verbse"})
	var usageErr *UsageError
	if !errors.As(err, &usageErr) {
		t.Fatalf("suggested error = %T, want UsageError", err)
	}
}

// TestRenderRowsHandlesUnicodeAndMultilineHelp protects help alignment from two
// subtle formatting cases: display width must be based on runes rather than UTF-8
// bytes, and continuation lines must align beneath the first help column.
func TestRenderRowsHandlesUnicodeAndMultilineHelp(t *testing.T) {
	rendered := renderRows([]helpRow{
		{label: "é", help: "first line\nsecond line"},
		{label: "ab", help: "other"},
	})
	lines := strings.Split(rendered, "\n")
	if len(lines) != 3 || lines[0] != "  é   first line" || !strings.HasPrefix(lines[1], strings.Repeat(" ", 6)+"second line") {
		t.Fatalf("unicode/multiline rows = %q", rendered)
	}
}
