package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type testHandler struct {
	Repository string   `arg:"repository" help:"repository to operate on"`
	Directory  string   `arg:"directory" help:"optional working directory"`
	Files      []string `arg:"files" help:"files to process"`

	Label    string        `flag:"label,l" help:"a label for the operation"`
	Verbose  bool          `flag:"verbose,v" help:"write detailed output"`
	Attempts testAttempts  `flag:"attempts,a" help:"number of attempts"`
	Ratio    float32       `flag:"ratio" help:"sampling ratio"`
	Timeout  time.Duration `flag:"timeout,t" help:"request timeout"`
	Tags     []string      `flag:"tag" help:"tag to attach; repeatable"`
	Mode     testMode      `flag:"mode" help:"operating mode"`

	description string
	help        string
	validateErr error
	runErr      error
	calls       []string
	gotContext  context.Context
}

type testAttempts int

type testMode struct {
	value string
}

func (mode *testMode) Set(value string) error {
	mode.value = strings.ToUpper(value)
	return nil
}

func (mode testMode) String() string {
	return mode.value
}

func (testMode) Type() string {
	return "mode"
}

func (handler *testHandler) Description() string {
	if handler.description == "" {
		return "Run a test command"
	}
	return handler.description
}

func (handler *testHandler) Help() string {
	return handler.help
}

func (handler *testHandler) Validate() error {
	handler.calls = append(handler.calls, "validate")
	return handler.validateErr
}

func (handler *testHandler) Run(ctx context.Context) error {
	handler.calls = append(handler.calls, "run")
	handler.gotContext = ctx
	return handler.runErr
}

type simpleHandler struct {
	description string
	help        string
	validateErr error
	runErr      error
	calls       []string
	gotContext  context.Context
}

func (handler *simpleHandler) Description() string {
	if handler.description == "" {
		return "Run a simple command"
	}
	return handler.description
}

func (handler *simpleHandler) Help() string {
	return handler.help
}

func (handler *simpleHandler) Validate() error {
	handler.calls = append(handler.calls, "validate")
	return handler.validateErr
}

func (handler *simpleHandler) Run(ctx context.Context) error {
	handler.calls = append(handler.calls, "run")
	handler.gotContext = ctx
	return handler.runErr
}

type nameHandler struct {
	Name string `arg:"name" help:"name to use"`

	description string
	help        string
}

func (handler *nameHandler) Description() string { return handler.description }
func (handler *nameHandler) Help() string        { return handler.help }
func (*nameHandler) Validate() error             { return nil }
func (*nameHandler) Run(context.Context) error   { return nil }

type repeatableOptionsHandler struct {
	Options []string `flag:"-,o" help:"option to apply; repeatable"`
}

func (*repeatableOptionsHandler) Description() string       { return "Apply repeated options" }
func (*repeatableOptionsHandler) Run(context.Context) error { return nil }

// TestRepeatableShortOnlyFlag protects DAC's short-only repeatable options.
// Each occurrence must append rather than overwrite so callers can supply
// multiple values without exposing a synthetic long name.
func TestRepeatableShortOnlyFlag(t *testing.T) {
	output := &bytes.Buffer{}
	handler := &repeatableOptionsHandler{}
	app := New(context.Background(), WithName("app"), WithOutput(output))
	app.MustAddCommand("cmd", handler)

	if err := app.Run([]string{"cmd", "-o", "Option1", "-o", "Option2"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got, want := strings.Join(handler.Options, ","), "Option1,Option2"; got != want {
		t.Fatalf("Options = %q, want %q", got, want)
	}
}

// TestShortOnlyFlagHelpDoesNotExposeInternalLongName keeps an implementation
// detail out of the public CLI. The parser invents a long name internally, but
// help must advertise only -o and its repeatable string value type.
func TestShortOnlyFlagHelpDoesNotExposeInternalLongName(t *testing.T) {
	output := &bytes.Buffer{}
	app := New(context.Background(), WithName("app"), WithOutput(output))
	app.MustAddCommand("cmd", &repeatableOptionsHandler{})

	if err := app.Run([]string{"cmd", "--help"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if help := output.String(); !strings.Contains(help, "-o") || !strings.Contains(help, "stringArray") || strings.Contains(help, "-o, --") || strings.Contains(help, shortOnlyFlagPrefix) {
		t.Fatalf("short-only flag help = %q", help)
	}
}

// TestRunBindsArgumentsAndFlags is the end-to-end binding contract for a leaf
// command. It mixes required, optional, and variadic positionals with scalar,
// repeatable, duration, and custom flags to catch ordering or conversion bugs;
// it also pins lifecycle order, context identity, and silent successful output.
func TestRunBindsArgumentsAndFlags(t *testing.T) {
	type contextKey string
	ctx := context.WithValue(context.Background(), contextKey("key"), "value")
	output := &bytes.Buffer{}
	handler := &testHandler{Attempts: 2}
	app := New(ctx, WithName("tool"), WithOutput(output))
	app.MustAddCommand("run <repository> [directory] [files...]", handler)

	err := app.Run([]string{
		"run",
		"--label=release",
		"origin",
		"--tag", "one",
		"--attempts", "4",
		"-v",
		"workspace",
		"--ratio", "0.25",
		"--timeout", "2s",
		"--tag=two",
		"--mode", "fast",
		"first.go",
		"second.go",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if handler.Repository != "origin" || handler.Directory != "workspace" {
		t.Fatalf("positional fields = (%q, %q), want (origin, workspace)", handler.Repository, handler.Directory)
	}
	if got, want := strings.Join(handler.Files, ","), "first.go,second.go"; got != want {
		t.Fatalf("Files = %q, want %q", got, want)
	}
	if handler.Label != "release" || !handler.Verbose || handler.Attempts != 4 || handler.Ratio != 0.25 {
		t.Fatalf("scalar flags were not bound: %v", handler)
	}
	if handler.Timeout != 2*time.Second || strings.Join(handler.Tags, ",") != "one,two" || handler.Mode.value != "FAST" {
		t.Fatalf("special flags were not bound: timeout=%v tags=%v mode=%q", handler.Timeout, handler.Tags, handler.Mode.value)
	}
	if got, want := strings.Join(handler.calls, ","), "validate,run"; got != want {
		t.Fatalf("calls = %q, want %q", got, want)
	}
	if handler.gotContext != ctx {
		t.Fatal("Run received a different context")
	}
	if output.Len() != 0 {
		t.Fatalf("normal invocation wrote help: %q", output.String())
	}
}

// TestRunHonoursDoubleDashAndOptionalZeroValue ensures users can pass values
// that look like flags. After --, even --help must bind as a positional, while
// omitted optional fields retain their Go zero values.
func TestRunHonoursDoubleDashAndOptionalZeroValue(t *testing.T) {
	handler := &testHandler{}
	app := New(context.Background(), WithOutput(&bytes.Buffer{}))
	app.MustAddCommand("run <repository> [directory] [files...]", handler)

	if err := app.Run([]string{"run", "origin", "--", "--help", "last.go"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if handler.Directory != "--help" || strings.Join(handler.Files, ",") != "last.go" {
		t.Fatalf("arguments after -- were not positional: directory=%q files=%v", handler.Directory, handler.Files)
	}
}

// TestArgumentArityAndFlagDefaults defines the command pattern's cardinality
// rules and ensures parsing does not erase defaults placed on the handler.
func TestArgumentArityAndFlagDefaults(t *testing.T) {
	// A required positional is part of the invocation contract, so omitting it
	// is a usage error rather than a handler validation or runtime error.
	missingHandler := &testHandler{}
	missingApp := New(context.Background(), WithOutput(&bytes.Buffer{}))
	missingApp.MustAddCommand("run <repository> [directory] [files...]", missingHandler)
	if err := missingApp.Run([]string{"run"}); !isUsageError(err) {
		t.Fatalf("missing argument error = %v, want UsageError", err)
	}

	// Optional and variadic arguments may be absent, and an omitted flag must
	// preserve the handler's preconfigured default instead of assigning zero.
	defaultHandler := &testHandler{Attempts: 3}
	defaultApp := New(context.Background(), WithOutput(&bytes.Buffer{}))
	defaultApp.MustAddCommand("run <repository> [directory] [files...]", defaultHandler)
	if err := defaultApp.Run([]string{"run", "origin"}); err != nil {
		t.Fatalf("default invocation error = %v", err)
	}
	if defaultHandler.Directory != "" || len(defaultHandler.Files) != 0 || defaultHandler.Attempts != 3 {
		t.Fatalf("optional/default values = directory=%q files=%v attempts=%d", defaultHandler.Directory, defaultHandler.Files, defaultHandler.Attempts)
	}

	// A fixed-arity pattern must not silently drop surplus positionals, which
	// commonly indicates a misspelled flag or an incorrectly registered command.
	extraApp := New(context.Background(), WithOutput(&bytes.Buffer{}))
	extraApp.MustAddCommand("run <name>", &nameHandler{description: "Run once"})
	if err := extraApp.Run([]string{"run", "one", "two"}); !isUsageError(err) {
		t.Fatalf("extra argument error = %v, want UsageError", err)
	}
}

// TestRunReturnsUsageErrorsWithoutWriting separates error production from UI
// policy. Run returns a UsageError carrying command-specific help, but the DAC
// application decides where and how to render it; the CLI library must not
// write duplicate diagnostics itself.
func TestRunReturnsUsageErrorsWithoutWriting(t *testing.T) {
	output := &bytes.Buffer{}
	handler := &simpleHandler{}
	app := New(context.Background(), WithName("tool"), WithOutput(output))
	app.MustAddCommand("run", handler)

	err := app.Run([]string{"run", "--unknown"})
	var usageErr *UsageError
	if !errors.As(err, &usageErr) {
		t.Fatalf("Run() error = %T %v, want *UsageError", err, err)
	}
	if !strings.Contains(usageErr.Usage(), "Usage:\n  tool run") {
		t.Fatalf("usage = %q, want command usage", usageErr.Usage())
	}
	if output.Len() != 0 {
		t.Fatalf("usage error wrote output: %q", output.String())
	}
}

// TestRunValidationAndRuntimeErrors preserves the distinction used for DAC exit
// codes: invalid user configuration is a usage error with help, while failures
// during an otherwise valid operation retain their original runtime identity.
func TestRunValidationAndRuntimeErrors(t *testing.T) {
	// Validation failure must stop before Run and remain discoverable through
	// errors.Is after the library adds usage context.
	validationFailure := errors.New("label is required")
	validationHandler := &simpleHandler{validateErr: validationFailure}
	validationApp := New(context.Background(), WithOutput(&bytes.Buffer{}))
	validationApp.MustAddCommand("run", validationHandler)

	err := validationApp.Run([]string{"run"})
	var usageErr *UsageError
	if !errors.As(err, &usageErr) || !errors.Is(err, validationFailure) {
		t.Fatalf("validation error = %v, want wrapped UsageError containing %v", err, validationFailure)
	}
	if got, want := strings.Join(validationHandler.calls, ","), "validate"; got != want {
		t.Fatalf("validation calls = %q, want %q", got, want)
	}

	// A runtime failure occurs after validation and must not be mislabeled as a
	// usage mistake, which would produce the wrong exit code and remediation.
	runtimeFailure := errors.New("network unavailable")
	runtimeHandler := &simpleHandler{runErr: runtimeFailure}
	runtimeApp := New(context.Background(), WithOutput(&bytes.Buffer{}))
	runtimeApp.MustAddCommand("run", runtimeHandler)

	err = runtimeApp.Run([]string{"run"})
	if !errors.Is(err, runtimeFailure) {
		t.Fatalf("runtime error = %v, want %v", err, runtimeFailure)
	}
	if _, isUsageError := err.(*UsageError); isUsageError {
		t.Fatalf("runtime error was wrapped as UsageError: %v", err)
	}
}

// TestHelpRendersAndBypassesHandler makes help a side-effect-free introspection
// path. Generated output must include syntax and handler documentation without
// invoking validation or runtime code that may access files or the network.
func TestHelpRendersAndBypassesHandler(t *testing.T) {
	output := &bytes.Buffer{}
	handler := &testHandler{
		description: "Run a remote operation",
		help:        "This command explains the remote operation.",
	}
	app := New(context.Background(), WithName("tool"), WithOutput(output))
	app.MustAddCommand("run <repository> [directory] [files...]", handler)

	if err := app.Run([]string{"run", "--help"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	help := output.String()
	for _, expected := range []string{
		"Usage:\n  tool run [flags] <repository> [directory] [files...]",
		"Description:\n  Run a remote operation",
		"This command explains the remote operation.",
		"Arguments:",
		"Options:",
		"-h, --help",
		"--label string",
	} {
		if !strings.Contains(help, expected) {
			t.Fatalf("generated help does not contain %q:\n%s", expected, help)
		}
	}
	if len(handler.calls) != 0 {
		t.Fatalf("help called handler lifecycle methods: %v", handler.calls)
	}
}

// TestRootGroupAndHelpCommand documents help routing at every command-tree
// depth. This keeps discovery useful whether a user asks for the whole CLI, a
// namespace of commands, or one runnable leaf.
func TestRootGroupAndHelpCommand(t *testing.T) {
	newApp := func(output *bytes.Buffer) *App {
		app := New(context.Background(), WithName("tool"), WithOutput(output))
		app.MustAddCommand("remote add <name>", &nameHandler{description: "Add a remote"})
		app.MustAddCommand("remote remove <name>", &nameHandler{description: "Remove a remote"})
		return app
	}

	// An empty invocation displays root help and includes every reachable leaf
	// when no explicit group metadata replaces that flattened view.
	rootOutput := &bytes.Buffer{}
	if err := newApp(rootOutput).Run(nil); err != nil {
		t.Fatalf("root help error = %v", err)
	}
	if !strings.Contains(rootOutput.String(), "remote add") || !strings.Contains(rootOutput.String(), "remote remove") {
		t.Fatalf("root help did not list descendant commands:\n%s", rootOutput.String())
	}

	// `help remote` targets the intermediate group and describes the next token
	// as a command rather than selecting an arbitrary descendant.
	groupOutput := &bytes.Buffer{}
	if err := newApp(groupOutput).Run([]string{"help", "remote"}); err != nil {
		t.Fatalf("group help error = %v", err)
	}
	if !strings.Contains(groupOutput.String(), "Usage:\n  tool remote <command>") {
		t.Fatalf("group help usage = %q", groupOutput.String())
	}

	// A full help path resolves to the leaf and includes its own description.
	leafOutput := &bytes.Buffer{}
	if err := newApp(leafOutput).Run([]string{"help", "remote", "add"}); err != nil {
		t.Fatalf("leaf help error = %v", err)
	}
	if !strings.Contains(leafOutput.String(), "Description:\n  Add a remote") {
		t.Fatalf("leaf help = %q", leafOutput.String())
	}
}

// TestRoutingPrefersLongestPathAndRejectsIncompletePathFlags protects command
// tree disambiguation. A literal child token must beat a parent's positional
// placeholder, and flags cannot interrupt a multi-token command path because
// their ownership is unknown until the leaf is selected.
func TestRoutingPrefersLongestPathAndRejectsIncompletePathFlags(t *testing.T) {
	parent := &nameHandler{description: "Use a remote"}
	child := &simpleHandler{description: "Add a remote"}
	app := New(context.Background(), WithOutput(&bytes.Buffer{}))
	app.MustAddCommand("remote <name>", parent)
	app.MustAddCommand("remote add", child)

	if err := app.Run([]string{"remote", "add"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if parent.Name != "" {
		t.Fatalf("parent received a positional command name: %q", parent.Name)
	}
	if got, want := strings.Join(child.calls, ","), "validate,run"; got != want {
		t.Fatalf("child calls = %q, want %q", got, want)
	}

	groupApp := New(context.Background(), WithOutput(&bytes.Buffer{}))
	groupApp.MustAddCommand("remote add", &simpleHandler{})
	err := groupApp.Run([]string{"remote", "--verbose", "add"})
	var usageErr *UsageError
	if !errors.As(err, &usageErr) || !strings.Contains(err.Error(), "flags must follow") {
		t.Fatalf("incomplete path flag error = %v, want UsageError", err)
	}
}

// TestRunIsSingleUse makes mutable handler binding explicit. Reusing an App
// would retain parsed values and lifecycle state, so a second run fails loudly
// rather than producing history-dependent behavior.
func TestRunIsSingleUse(t *testing.T) {
	app := New(context.Background(), WithOutput(&bytes.Buffer{}))
	app.MustAddCommand("run", &simpleHandler{})

	if err := app.Run([]string{"run"}); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if err := app.Run([]string{"run"}); !errors.Is(err, ErrAlreadyRun) {
		t.Fatalf("second Run() error = %v, want ErrAlreadyRun", err)
	}
}

// TestAddCommandRejectsInvalidPatternsAndBindings catches programmer mistakes
// during command registration, before an end user encounters an ambiguous or
// unusable CLI. Each invalid shape exercises a distinct parser invariant.
func TestAddCommandRejectsInvalidPatternsAndBindings(t *testing.T) {
	invalidPatterns := []string{
		"",                             // A command needs at least one literal path component.
		"%flags% run",                  // The internal flags marker is not valid user syntax.
		"<name>",                       // A path cannot begin with a positional placeholder.
		"run [directory] <repository>", // Required arguments cannot follow optional ones.
		"run <files...> [directory]",   // A variadic argument must be last or it consumes the rest.
		"run <name> again",             // Literal path tokens cannot follow argument bindings.
		"help",                         // The built-in help command is reserved by the framework.
	}
	for _, pattern := range invalidPatterns {
		t.Run(pattern, func(t *testing.T) {
			app := New(context.Background(), WithOutput(&bytes.Buffer{}))
			if err := app.AddCommand(pattern, &simpleHandler{}); err == nil {
				t.Fatalf("AddCommand(%q) succeeded, want error", pattern)
			}
		})
	}

	// Duplicate paths would make dispatch dependent on registration order.
	app := New(context.Background(), WithOutput(&bytes.Buffer{}))
	app.MustAddCommand("run", &simpleHandler{})
	if err := app.AddCommand("run", &simpleHandler{}); err == nil {
		t.Fatal("duplicate command registration succeeded")
	}

	// Tagged fields must correspond to pattern arguments so a typo cannot leave
	// an apparently supported handler field permanently unset.
	if err := New(context.Background()).AddCommand("run", &untaggedArgumentHandler{}); err == nil {
		t.Fatal("missing arg binding succeeded")
	}
	// Unsupported collection types are rejected at registration, not during a
	// user's invocation after other command side effects may have occurred.
	if err := New(context.Background()).AddCommand("run", &badFlagHandler{}); err == nil {
		t.Fatal("unsupported flag binding succeeded")
	}
	// User flags cannot shadow the built-in --help behavior.
	if err := New(context.Background()).AddCommand("run", &helpFlagHandler{}); err == nil {
		t.Fatal("help flag collision succeeded")
	}
}

// isUsageError checks the public wrapping contract rather than requiring the
// UsageError to be the outermost concrete error.
func isUsageError(err error) bool {
	var usageErr *UsageError
	return errors.As(err, &usageErr)
}

type untaggedArgumentHandler struct {
	Name string `arg:"name"`
}

func (*untaggedArgumentHandler) Description() string       { return "Broken handler" }
func (*untaggedArgumentHandler) Help() string              { return "" }
func (*untaggedArgumentHandler) Validate() error           { return nil }
func (*untaggedArgumentHandler) Run(context.Context) error { return nil }

type badFlagHandler struct {
	Values []int `flag:"value"`
}

func (*badFlagHandler) Description() string       { return "Broken handler" }
func (*badFlagHandler) Help() string              { return "" }
func (*badFlagHandler) Validate() error           { return nil }
func (*badFlagHandler) Run(context.Context) error { return nil }

type helpFlagHandler struct {
	Requested bool `flag:"help"`
}

func (*helpFlagHandler) Description() string       { return "Broken handler" }
func (*helpFlagHandler) Help() string              { return "" }
func (*helpFlagHandler) Validate() error           { return nil }
func (*helpFlagHandler) Run(context.Context) error { return nil }
