package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/spf13/pflag"
)

// CommandHandler defines the minimum behavior required for a command. Optional
// help and validation behavior are discovered through HelpProvider and Validator.
type CommandHandler interface {
	Description() string
	Run(context.Context) error
}

// HelpProvider supplies optional long-form command documentation.
type HelpProvider interface {
	Help() string
}

// Validator validates a fully bound command before it is run.
type Validator interface {
	Validate() error
}

// Option configures an App during construction.
type Option func(*App)

// WithName sets the executable name used in generated usage and help text.
func WithName(name string) Option {
	return func(app *App) {
		if strings.TrimSpace(name) == "" {
			panic("cli: output name must not be empty")
		}
		app.name = name
	}
}

// WithOutput sets the writer used for help, version, and completion output.
func WithOutput(output io.Writer) Option {
	return func(app *App) {
		if output == nil {
			panic("cli: output writer must not be nil")
		}
		app.output = output
	}
}

// WithErrorOutput sets the writer used for non-fatal warnings such as command
// deprecations. Returned errors remain the caller's responsibility.
func WithErrorOutput(output io.Writer) Option {
	return func(app *App) {
		if output == nil {
			panic("cli: error output writer must not be nil")
		}
		app.errorOutput = output
	}
}

// WithVersion enables a root --version flag that prints the supplied version.
func WithVersion(version string) Option {
	return func(app *App) {
		version = strings.TrimSpace(version)
		if version == "" {
			panic("cli: version must not be empty")
		}
		app.version = version
	}
}

// WithCompletion enables the completion command and hidden dynamic-completion
// protocol used by the generated bash, zsh, and fish scripts.
func WithCompletion() Option {
	return func(app *App) {
		app.completion = true
	}
}

// CommandOption configures aliases and lifecycle metadata for one command.
type CommandOption func(*commandConfig) error

type commandConfig struct {
	aliases    []string
	deprecated string
}

// WithAliases registers alternate command paths that use the canonical
// command's arguments, flags, validation, and handler.
func WithAliases(patterns ...string) CommandOption {
	return func(config *commandConfig) error {
		if len(patterns) == 0 {
			return newCLIError(ErrInvalidCommandDefinition, "cli: aliases must not be empty")
		}
		config.aliases = append(config.aliases, patterns...)
		return nil
	}
}

// WithDeprecated marks a command as deprecated and supplies the migration
// message shown in help and immediately before command execution.
func WithDeprecated(message string) CommandOption {
	return func(config *commandConfig) error {
		message = strings.TrimSpace(message)
		if message == "" {
			return newCLIError(ErrInvalidCommandDefinition, "cli: deprecation message must not be empty")
		}
		config.deprecated = message
		return nil
	}
}

// GroupOption configures documentation for a command group.
type GroupOption func(*groupConfig) error

type groupConfig struct {
	help string
}

// WithGroupHelp supplies optional long-form documentation for a command group.
func WithGroupHelp(help string) GroupOption {
	return func(config *groupConfig) error {
		config.help = strings.TrimSpace(help)
		return nil
	}
}

// App registers command handlers and dispatches one command-line invocation.
type App struct {
	ctx         context.Context
	name        string
	output      io.Writer
	errorOutput io.Writer
	version     string
	completion  bool
	root        *commandNode
	globals     *pflag.FlagSet

	runMu sync.Mutex
	ran   bool
}

// New creates an App. The default name is the executable base name; generated
// output goes to stdout and warnings go to stderr.
func New(ctx context.Context, options ...Option) *App {
	if ctx == nil {
		panic("cli: context must not be nil")
	}

	app := &App{
		ctx:         ctx,
		name:        filepath.Base(os.Args[0]),
		output:      os.Stdout,
		errorOutput: os.Stderr,
		root: &commandNode{
			children: make(map[string]*commandNode),
		},
	}
	for _, option := range options {
		if option == nil {
			panic("cli: option must not be nil")
		}
		option(app)
	}

	return app
}

// AddCommand registers handler for pattern. Registration validates the pattern,
// aliases, and tagged handler fields before modifying the command tree.
func (app *App) AddCommand(pattern string, handler CommandHandler, options ...CommandOption) error {
	command, err := newCommand(pattern, handler, options...)
	if err != nil {
		return err
	}
	if err := app.validateFlagConflicts(command.flags); err != nil {
		return err
	}

	paths := append([][]string{command.path}, command.aliases...)
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		key := strings.Join(path, " ")
		if _, exists := seen[key]; exists {
			return newCLIError(ErrInvalidCommandDefinition, "cli: command path %q is declared more than once", key)
		}
		seen[key] = struct{}{}

		if node := app.findNode(path); node != nil && node.command != nil {
			return newCLIError(ErrInvalidCommandDefinition, "cli: command path %q is already registered", key)
		}
	}

	for _, path := range paths {
		app.ensureNode(path).command = command
	}
	return nil
}

// MustAddCommand registers handler or panics when its definition is invalid.
func (app *App) MustAddCommand(pattern string, handler CommandHandler, options ...CommandOption) {
	if err := app.AddCommand(pattern, handler, options...); err != nil {
		panic(err)
	}
}

// AddGroup documents a command path that exists only to contain child commands.
func (app *App) AddGroup(pathValue, description string, options ...GroupOption) error {
	path, err := parseCommandPath(pathValue)
	if err != nil {
		return err
	}
	description = strings.TrimSpace(description)
	if description == "" {
		return newCLIError(ErrInvalidCommandDefinition, "cli: group %q must provide a non-empty description", pathValue)
	}

	config := groupConfig{}
	for _, option := range options {
		if option == nil {
			return newCLIError(ErrInvalidCommandDefinition, "cli: group option must not be nil")
		}
		if err := option(&config); err != nil {
			return err
		}
	}
	if node := app.findNode(path); node != nil && node.group != nil {
		return newCLIError(ErrInvalidCommandDefinition, "cli: group path %q is already registered", strings.Join(path, " "))
	}

	app.ensureNode(path).group = &commandGroup{description: description, help: config.help}
	return nil
}

// MustAddGroup registers group documentation or panics when it is invalid.
func (app *App) MustAddGroup(path, description string, options ...GroupOption) {
	if err := app.AddGroup(path, description, options...); err != nil {
		panic(err)
	}
}

// AddGlobalFlags binds tagged fields from target as flags accepted before or
// after every command path. Only one global target may be registered.
func (app *App) AddGlobalFlags(target any) error {
	if app.globals != nil {
		return newCLIError(ErrInvalidCommandDefinition, "cli: global flags are already registered")
	}

	value := reflect.ValueOf(target)
	if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() || value.Elem().Kind() != reflect.Struct {
		return newCLIError(ErrInvalidCommandDefinition, "cli: global flags target must be a non-nil pointer to a struct")
	}
	arguments, bindings, err := inspectHandlerFields(value.Elem().Type())
	if err != nil {
		return err
	}
	if len(arguments) != 0 {
		return newCLIError(ErrInvalidCommandDefinition, "cli: global flags target must not contain arg-tagged fields")
	}

	flags := pflag.NewFlagSet(app.name, pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	for _, binding := range bindings {
		if binding.name == "version" {
			return newCLIError(ErrInvalidCommandDefinition, "cli: global flag %q conflicts with built-in version flag", binding.name)
		}
		if err := addFlag(flags, value.Elem().Field(binding.fieldIndex), binding); err != nil {
			return err
		}
	}
	if err := app.validateExistingCommandFlags(flags); err != nil {
		return err
	}

	app.globals = flags
	return nil
}

// MustAddGlobalFlags registers global flags or panics when their definition is invalid.
func (app *App) MustAddGlobalFlags(target any) {
	if err := app.AddGlobalFlags(target); err != nil {
		panic(err)
	}
}

// Run dispatches args, parses flags, and runs the matched handler. It writes only
// explicitly requested generated output and returns all usage/runtime failures.
func (app *App) Run(args []string) error {
	if !app.claimRun() {
		return ErrAlreadyRun
	}

	remaining, err := app.extractGlobalFlags(args)
	if err != nil {
		return app.usageError(app.root, app.suggestFlagError(app.root, err))
	}
	if len(remaining) == 0 || isHelpFlag(remaining[0]) {
		return app.writeHelp(app.root)
	}
	if remaining[0] == "--version" && app.version != "" {
		if len(remaining) != 1 {
			return app.usageError(app.root, newCLIError(ErrInvalidInvocation, "--version does not accept arguments"))
		}
		return app.writeVersion()
	}
	if remaining[0] == "help" {
		return app.runHelp(remaining[1:])
	}
	if remaining[0] == "completion" && app.completion {
		return app.runCompletion(remaining[1:])
	}
	if remaining[0] == "__complete" && app.completion {
		return app.writeCompletionCandidates(remaining[1:])
	}

	node, consumed := app.matchPath(remaining)
	commandArgs := remaining[consumed:]
	if node == app.root {
		if strings.HasPrefix(remaining[0], "-") {
			return app.usageError(node, app.suggestFlagError(node, newCLIError(ErrInvalidInvocation, "unknown flag %q", remaining[0])))
		}
		return app.usageError(node, app.unknownCommandError(node, remaining[0]))
	}
	if node.command == nil {
		if len(commandArgs) == 0 || isHelpFlag(commandArgs[0]) {
			return app.writeHelp(node)
		}
		if strings.HasPrefix(commandArgs[0], "-") {
			return app.usageError(node, newCLIError(ErrInvalidInvocation, "flags must follow a complete command path"))
		}
		return app.usageError(node, app.unknownCommandError(node, commandArgs[0]))
	}

	return app.runCommand(node, commandArgs)
}

// UsageError identifies a command-line usage failure. Usage remains separate
// from Error so applications can render failures and select an exit code.
type UsageError struct {
	err   error
	usage string
}

// Error reports the underlying command-line failure.
func (err *UsageError) Error() string { return err.err.Error() }

// Unwrap exposes the parsing or validation error that caused the usage error.
func (err *UsageError) Unwrap() error { return err.err }

// Usage returns the relevant generated usage block.
func (err *UsageError) Usage() string { return err.usage }

type commandNode struct {
	segment  string
	parent   *commandNode
	children map[string]*commandNode
	command  *command
	group    *commandGroup
}

type commandGroup struct {
	description string
	help        string
}

// claimRun atomically makes an App single-use before any bound fields change.
func (app *App) claimRun() bool {
	app.runMu.Lock()
	defer app.runMu.Unlock()
	if app.ran {
		return false
	}
	app.ran = true
	return true
}

// findNode returns an existing node without mutating the command tree.
func (app *App) findNode(path []string) *commandNode {
	node := app.root
	for _, segment := range path {
		node = node.children[segment]
		if node == nil {
			return nil
		}
	}
	return node
}

// ensureNode creates the minimum missing trie nodes for path.
func (app *App) ensureNode(path []string) *commandNode {
	node := app.root
	for _, segment := range path {
		child := node.children[segment]
		if child == nil {
			child = &commandNode{segment: segment, parent: node, children: make(map[string]*commandNode)}
			node.children[segment] = child
		}
		node = child
	}
	return node
}

// matchPath selects the longest registered path prefix.
func (app *App) matchPath(args []string) (*commandNode, int) {
	node := app.root
	consumed := 0
	for consumed < len(args) {
		child := node.children[args[consumed]]
		if child == nil {
			break
		}
		node = child
		consumed++
	}
	return node, consumed
}

// runHelp resolves groups separately so anonymous and documented groups remain addressable.
func (app *App) runHelp(path []string) error {
	if len(path) == 1 && path[0] == "completion" && app.completion {
		_, err := fmt.Fprintf(app.output, "Usage:\n  %s completion <bash|zsh|fish>\n\nDescription:\n  Generate a shell completion script.\n", app.name)
		return err
	}

	node := app.root
	for _, segment := range path {
		if isHelpFlag(segment) {
			return app.usageError(node, newCLIError(ErrInvalidInvocation, "help accepts a command path, not flag %q", segment))
		}
		child := node.children[segment]
		if child == nil {
			return app.usageError(node, app.unknownCommandError(node, segment))
		}
		node = child
	}
	return app.writeHelp(node)
}

// runCommand parses local flags, binds typed arguments, validates, and executes.
func (app *App) runCommand(node *commandNode, args []string) error {
	command := node.command
	parsedFlags, err := app.parseCommandFlags(command, args)
	if err != nil {
		return app.usageError(node, app.suggestFlagError(node, err))
	}
	if command.helpRequested {
		return app.writeHelp(node)
	}
	if err := command.bindArguments(parsedFlags.Args()); err != nil {
		return app.usageError(node, err)
	}
	if validator, ok := command.handler.(Validator); ok {
		if err := validator.Validate(); err != nil {
			return app.usageError(node, wrapCLIError(ErrInvalidInvocation, err, "validate command: %v", err))
		}
	}
	if command.deprecated != "" {
		if _, err := fmt.Fprintf(app.errorOutput, "Warning: command %q is deprecated: %s\n", strings.Join(node.path(), " "), command.deprecated); err != nil {
			return wrapCLIError(ErrOutput, err, "write deprecation warning: %v", err)
		}
	}
	return command.handler.Run(app.ctx)
}

// usageError couples an error to the narrowest useful usage block.
func (app *App) usageError(node *commandNode, err error) error {
	return &UsageError{err: err, usage: app.renderUsage(node)}
}

// writeHelp renders help only after an explicit request.
func (app *App) writeHelp(node *commandNode) error {
	_, err := io.WriteString(app.output, app.renderHelp(node))
	return err
}

// writeVersion renders a stable, script-friendly version line.
func (app *App) writeVersion() error {
	_, err := fmt.Fprintf(app.output, "%s %s\n", app.name, app.version)
	return err
}

// renderUsage describes global options, the current path, and its next valid tokens.
func (app *App) renderUsage(node *commandNode) string {
	parts := []string{app.name}
	if app.globalFlagCount() > 0 {
		parts = append(parts, "[global flags]")
	}
	parts = append(parts, node.path()...)
	if node.command == nil {
		parts = append(parts, "<command>")
	} else {
		if node.command.flagCount > 0 {
			parts = append(parts, "[flags]")
		}
		for _, argument := range node.command.arguments {
			parts = append(parts, argument.syntax())
		}
	}
	return "Usage:\n  " + strings.Join(parts, " ") + "\n"
}

// renderHelp builds independent sections so optional metadata never disturbs spacing.
func (app *App) renderHelp(node *commandNode) string {
	sections := []string{strings.TrimSuffix(app.renderUsage(node), "\n")}
	if node.command != nil {
		sections = append(sections, "Description:\n  "+node.command.description)
		if node.command.help != "" {
			sections = append(sections, "Help:\n"+indent(node.command.help, "  "))
		}
		if len(node.command.aliases) > 0 {
			aliases := make([]string, 0, len(node.command.aliases))
			for _, alias := range node.command.aliases {
				aliases = append(aliases, strings.Join(alias, " "))
			}
			sections = append(sections, "Aliases:\n  "+strings.Join(aliases, ", "))
		}
		if node.command.deprecated != "" {
			sections = append(sections, "Deprecated:\n  "+node.command.deprecated)
		}
		if len(node.command.arguments) > 0 {
			rows := make([]helpRow, 0, len(node.command.arguments))
			for _, argument := range node.command.arguments {
				rows = append(rows, helpRow{label: argument.syntax(), help: argument.help})
			}
			sections = append(sections, "Arguments:\n"+renderRows(rows))
		}
		if options := strings.TrimRight(flagUsages(node.command.flags), "\n"); options != "" {
			sections = append(sections, "Options:\n"+options)
		}
	} else if node.group != nil {
		sections = append(sections, "Description:\n  "+node.group.description)
		if node.group.help != "" {
			sections = append(sections, "Help:\n"+indent(node.group.help, "  "))
		}
	}
	if node.command == nil {
		options := []helpRow{{label: "-h, --help", help: "show help"}}
		if node == app.root && app.version != "" {
			options = append(options, helpRow{label: "--version", help: "show version"})
		}
		sections = append(sections, "Options:\n"+renderRows(options))
	}
	if app.globals != nil {
		if options := strings.TrimRight(flagUsages(app.globals), "\n"); options != "" {
			sections = append(sections, "Global options:\n"+options)
		}
	}
	if rows := app.commandRows(node); len(rows) > 0 {
		sections = append(sections, "Commands:\n"+renderRows(rows))
	}
	return strings.Join(sections, "\n\n") + "\n"
}

// flagUsages removes pflag's private long-name placeholder when rendering a
// short-only flag while preserving its column alignment with other options.
func flagUsages(flags *pflag.FlagSet) string {
	usage := flags.FlagUsagesWrapped(0)
	flags.VisitAll(func(flag *pflag.Flag) {
		if !isShortOnlyFlag(flag) {
			return
		}

		generated := "  -" + flag.Shorthand + ", --" + flag.Name
		displayed := "  -" + flag.Shorthand + strings.Repeat(" ", len(generated)-len(flag.Shorthand)-3)
		usage = strings.ReplaceAll(usage, generated, displayed)
	})
	return usage
}

// commandRows lists documented children directly and expands anonymous grouping
// nodes so every runnable command remains discoverable.
func (app *App) commandRows(node *commandNode) []helpRow {
	var rows []helpRow
	var collect func(*commandNode, []string)
	collect = func(parent *commandNode, prefix []string) {
		segments := make([]string, 0, len(parent.children))
		for segment := range parent.children {
			segments = append(segments, segment)
		}
		sort.Strings(segments)
		for _, segment := range segments {
			child := parent.children[segment]
			labelPath := append(append([]string(nil), prefix...), segment)
			if child.command != nil && !samePath(child.path(), child.command.path) && child.group == nil && len(child.children) == 0 {
				continue
			}
			switch {
			case child.command != nil:
				rows = append(rows, helpRow{label: strings.Join(labelPath, " "), help: child.command.description})
			case child.group != nil:
				rows = append(rows, helpRow{label: strings.Join(labelPath, " "), help: child.group.description})
			default:
				collect(child, labelPath)
			}
		}
	}
	collect(node, nil)
	if node == app.root {
		rows = append(rows, helpRow{label: "help", help: "Show help for a command"})
		if app.completion {
			rows = append(rows, helpRow{label: "completion", help: "Generate shell completion"})
		}
		sort.Slice(rows, func(left, right int) bool { return rows[left].label < rows[right].label })
	}
	return rows
}

// path reconstructs a node's invocation path from its trie parents.
func (node *commandNode) path() []string {
	var reversed []string
	for current := node; current.parent != nil; current = current.parent {
		reversed = append(reversed, current.segment)
	}
	path := make([]string, len(reversed))
	for index := range reversed {
		path[len(reversed)-1-index] = reversed[index]
	}
	return path
}

// samePath compares command paths without allocating joined strings.
func samePath(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func isHelpFlag(value string) bool { return value == "-h" || value == "--help" }

func indent(value, prefix string) string {
	return prefix + strings.ReplaceAll(value, "\n", "\n"+prefix)
}

type helpRow struct {
	label string
	help  string
}

// renderRows aligns compact labels while indenting multiline explanations.
func renderRows(rows []helpRow) string {
	width := 0
	for _, row := range rows {
		if labelWidth := utf8.RuneCountInString(row.label); labelWidth > width {
			width = labelWidth
		}
	}

	var builder strings.Builder
	for _, row := range rows {
		if row.help == "" {
			fmt.Fprintf(&builder, "  %s\n", row.label)
			continue
		}
		padding := strings.Repeat(" ", width-utf8.RuneCountInString(row.label))
		help := strings.ReplaceAll(row.help, "\n", "\n  "+strings.Repeat(" ", width)+"  ")
		fmt.Fprintf(&builder, "  %s%s  %s\n", row.label, padding, help)
	}
	return strings.TrimSuffix(builder.String(), "\n")
}
