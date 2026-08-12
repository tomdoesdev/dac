package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/spf13/pflag"
)

// globalFlagCount reports configured definitions rather than parsed values.
func (app *App) globalFlagCount() int {
	if app.globals == nil {
		return 0
	}
	count := 0
	app.globals.VisitAll(func(*pflag.Flag) { count++ })
	return count
}

// validateFlagConflicts prevents a command flag from being shadowed by a global
// flag when the two FlagSets are combined for parsing.
func (app *App) validateFlagConflicts(local *pflag.FlagSet) error {
	if app.globals == nil {
		return nil
	}
	var conflict error
	local.VisitAll(func(flag *pflag.Flag) {
		if conflict != nil || flag.Name == "help" {
			return
		}
		if app.globals.Lookup(flag.Name) != nil {
			conflict = newCLIError(ErrInvalidCommandDefinition, "cli: command flag %q conflicts with a global flag", flag.Name)
			return
		}
		if flag.Shorthand != "" && app.globals.ShorthandLookup(flag.Shorthand) != nil {
			conflict = newCLIError(ErrInvalidCommandDefinition, "cli: command shorthand %q conflicts with a global flag", flag.Shorthand)
		}
	})
	return conflict
}

// validateExistingCommandFlags applies conflict checks when globals are added
// after commands, so registration order cannot alter validation behavior.
func (app *App) validateExistingCommandFlags(globals *pflag.FlagSet) error {
	seen := make(map[*command]struct{})
	var visit func(*commandNode) error
	visit = func(node *commandNode) error {
		if node.command != nil {
			if _, exists := seen[node.command]; !exists {
				seen[node.command] = struct{}{}
				var conflict error
				globals.VisitAll(func(flag *pflag.Flag) {
					if conflict != nil {
						return
					}
					if node.command.flags.Lookup(flag.Name) != nil {
						conflict = newCLIError(ErrInvalidCommandDefinition, "cli: global flag %q conflicts with a command flag", flag.Name)
						return
					}
					if flag.Shorthand != "" && node.command.flags.ShorthandLookup(flag.Shorthand) != nil {
						conflict = newCLIError(ErrInvalidCommandDefinition, "cli: global shorthand %q conflicts with a command flag", flag.Shorthand)
					}
				})
				if conflict != nil {
					return conflict
				}
			}
		}
		for _, child := range node.children {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(app.root)
}

// extractGlobalFlags consumes only leading globals. Flags after a complete
// command path are parsed together with local flags, avoiding ambiguity about
// whether a flag-looking token is another flag's value.
func (app *App) extractGlobalFlags(args []string) ([]string, error) {
	if app.globals == nil {
		return args, nil
	}

	index := 0
	for index < len(args) {
		consumed, recognized, err := consumeFlag(app.globals, args[index:])
		if err != nil {
			return nil, err
		}
		if !recognized {
			break
		}
		index += consumed
	}
	return args[index:], nil
}

// consumeFlag parses one known long flag or short-flag cluster from the front
// of args. It leaves unknown tokens untouched for command routing or local parsing.
func consumeFlag(flags *pflag.FlagSet, args []string) (int, bool, error) {
	if len(args) == 0 || args[0] == "--" || len(args[0]) < 2 || args[0][0] != '-' {
		return 0, false, nil
	}
	token := args[0]
	if strings.HasPrefix(token, "--") {
		nameValue := strings.TrimPrefix(token, "--")
		parts := strings.SplitN(nameValue, "=", 2)
		flag := flags.Lookup(parts[0])
		if flag == nil {
			return 0, false, nil
		}
		if len(parts) == 2 {
			return 1, true, flags.Set(flag.Name, parts[1])
		}
		if flag.NoOptDefVal != "" {
			return 1, true, flags.Set(flag.Name, flag.NoOptDefVal)
		}
		if len(args) < 2 {
			return 0, true, newCLIError(ErrInvalidInvocation, "flag needs an argument: --%s", flag.Name)
		}
		return 2, true, flags.Set(flag.Name, args[1])
	}

	cluster := strings.TrimPrefix(token, "-")
	type assignment struct {
		name  string
		value string
	}
	var assignments []assignment
	consumed := 1
	for index := 0; index < len(cluster); index++ {
		flag := flags.ShorthandLookup(cluster[index : index+1])
		if flag == nil {
			return 0, false, nil
		}
		remainder := cluster[index+1:]
		if flag.NoOptDefVal != "" && !strings.HasPrefix(remainder, "=") {
			assignments = append(assignments, assignment{name: flag.Name, value: flag.NoOptDefVal})
			continue
		}
		if remainder != "" {
			assignments = append(assignments, assignment{name: flag.Name, value: strings.TrimPrefix(remainder, "=")})
			break
		}
		if len(args) < 2 {
			return 0, true, newCLIError(ErrInvalidInvocation, "flag needs an argument: -%s", flag.Shorthand)
		}
		assignments = append(assignments, assignment{name: flag.Name, value: args[1]})
		consumed = 2
		break
	}
	for _, assignment := range assignments {
		if err := flags.Set(assignment.name, assignment.value); err != nil {
			return 0, true, err
		}
	}
	return consumed, true, nil
}

// parseCommandFlags combines local and global definitions without copying the
// bound values they point at.
func (app *App) parseCommandFlags(command *command, args []string) (*pflag.FlagSet, error) {
	if app.globals == nil {
		if err := command.flags.Parse(args); err != nil {
			return nil, err
		}
		return command.flags, nil
	}

	flags := pflag.NewFlagSet(strings.Join(command.path, " "), pflag.ContinueOnError)
	flags.SetInterspersed(true)
	flags.SetOutput(io.Discard)
	flags.AddFlagSet(command.flags)
	flags.AddFlagSet(app.globals)
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	return flags, nil
}

// unknownCommandError adds a conservative edit-distance suggestion when one
// child is clearly closer than the rest.
func (app *App) unknownCommandError(node *commandNode, value string) error {
	candidates := make([]string, 0, len(node.children)+2)
	for segment := range node.children {
		candidates = append(candidates, segment)
	}
	if node == app.root {
		candidates = append(candidates, "help")
		if app.completion {
			candidates = append(candidates, "completion")
		}
	}
	if candidate := nearest(value, candidates); candidate != "" {
		return newCLIError(ErrInvalidInvocation, "unknown command %q; did you mean %q?", value, candidate)
	}
	return newCLIError(ErrInvalidInvocation, "unknown command %q", value)
}

// suggestFlagError enriches pflag and root-routing errors without replacing the
// original error in the unwrap chain.
func (app *App) suggestFlagError(node *commandNode, err error) error {
	value := unknownFlagValue(err.Error())
	if value == "" {
		return err
	}
	candidates := app.flagCandidates(node)
	if candidate := nearest(value, candidates); candidate != "" {
		return &suggestedError{err: err, suggestion: candidate}
	}
	return err
}

type suggestedError struct {
	err        error
	suggestion string
}

func (err *suggestedError) Error() string {
	return fmt.Sprintf("%s; did you mean %q?", err.err, err.suggestion)
}

func (err *suggestedError) Unwrap() error { return err.err }

// unknownFlagValue extracts the user-entered spelling from pflag's stable error
// forms and from root routing's quoted unknown-flag error.
func unknownFlagValue(message string) string {
	for _, prefix := range []string{"unknown flag: ", "unknown flag "} {
		if remainder, found := strings.CutPrefix(message, prefix); found {
			return strings.Trim(strings.Fields(remainder)[0], "'\"")
		}
	}
	if index := strings.Index(message, "unknown shorthand flag: '"); index >= 0 {
		remainder := message[index+len("unknown shorthand flag: '"):]
		if remainder != "" {
			return "-" + remainder[:1]
		}
	}
	return ""
}

// flagCandidates returns visible spellings accepted at a node.
func (app *App) flagCandidates(node *commandNode) []string {
	set := map[string]struct{}{"-h": {}, "--help": {}}
	if app.version != "" && node == app.root {
		set["--version"] = struct{}{}
	}
	addFlags := func(flags *pflag.FlagSet) {
		if flags == nil {
			return
		}
		flags.VisitAll(func(flag *pflag.Flag) {
			if !isShortOnlyFlag(flag) {
				set["--"+flag.Name] = struct{}{}
			}
			if flag.Shorthand != "" {
				set["-"+flag.Shorthand] = struct{}{}
			}
		})
	}
	addFlags(app.globals)
	if node.command != nil {
		addFlags(node.command.flags)
	}
	candidates := make([]string, 0, len(set))
	for value := range set {
		candidates = append(candidates, value)
	}
	return candidates
}

// nearest selects one close candidate and suppresses noisy guesses for distant
// input or ties.
func nearest(value string, candidates []string) string {
	best := ""
	limit := max(2, len(value)/3+1)
	bestDistance := limit + 1
	tied := false
	for _, candidate := range candidates {
		distance := editDistance(value, candidate)
		if distance < bestDistance {
			best, bestDistance, tied = candidate, distance, false
		} else if distance == bestDistance {
			tied = true
		}
	}
	if tied || bestDistance > limit {
		return ""
	}
	return best
}

// editDistance computes Levenshtein distance with two reusable rows.
func editDistance(left, right string) int {
	leftRunes, rightRunes := []rune(left), []rune(right)
	previous := make([]int, len(rightRunes)+1)
	current := make([]int, len(rightRunes)+1)
	for index := range previous {
		previous[index] = index
	}
	for leftIndex, leftRune := range leftRunes {
		current[0] = leftIndex + 1
		for rightIndex, rightRune := range rightRunes {
			cost := 1
			if leftRune == rightRune {
				cost = 0
			}
			current[rightIndex+1] = min(current[rightIndex]+1, previous[rightIndex+1]+1, previous[rightIndex]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(rightRunes)]
}

// Completion returns a dynamic completion script for a supported shell.
func (app *App) Completion(shell string) (string, error) {
	if !app.completion {
		return "", ErrCompletionDisabled
	}
	identifier := completionIdentifier(app.name)
	quotedName := shellQuote(app.name)
	switch shell {
	case "bash":
		return fmt.Sprintf(`_%s_completion() {
  local candidate
  COMPREPLY=()
  while IFS= read -r candidate; do
    COMPREPLY+=("$candidate")
  done < <("${COMP_WORDS[0]}" __complete "${COMP_WORDS[@]:1}")
}
complete -F _%s_completion %s
`, identifier, identifier, quotedName), nil
	case "zsh":
		return fmt.Sprintf(`_%s_completion() {
  local -a candidates
  candidates=("${(@f)$(${words[1]} __complete ${words[@]:2})}")
  _describe 'values' candidates
}
compdef _%s_completion %s
`, identifier, identifier, quotedName), nil
	case "fish":
		return fmt.Sprintf(`function __%s_completion
  set -l words (commandline -opc)
  set -l current (commandline -ct)
  command $words[1] __complete $words[2..-1] $current
end
complete -c %s -f -a '(__%s_completion)'
`, identifier, quotedName, identifier), nil
	default:
		return "", newCLIError(ErrUnsupportedShell, "unsupported shell %q; expected bash, zsh, or fish", shell)
	}
}

// runCompletion validates the built-in command and writes a generated script.
func (app *App) runCompletion(args []string) error {
	if len(args) != 1 {
		return app.usageError(app.root, newCLIError(ErrInvalidInvocation, "completion requires exactly one shell: bash, zsh, or fish"))
	}
	script, err := app.Completion(args[0])
	if err != nil {
		return app.usageError(app.root, err)
	}
	_, err = io.WriteString(app.output, script)
	return err
}

// writeCompletionCandidates serves the newline-delimited protocol consumed by
// generated shell scripts.
func (app *App) writeCompletionCandidates(args []string) error {
	for _, candidate := range app.completionCandidates(args) {
		if _, err := fmt.Fprintln(app.output, candidate); err != nil {
			return err
		}
	}
	return nil
}

// completionCandidates follows completed command tokens, skips known flags,
// and suggests the next child or visible option by prefix.
func (app *App) completionCandidates(args []string) []string {
	current := ""
	completed := args
	if len(args) > 0 {
		current = args[len(args)-1]
		completed = args[:len(args)-1]
	}

	node := app.root
	for index := 0; index < len(completed); {
		word := completed[index]
		if word == "--" {
			break
		}
		if consumed := app.completionFlagWidth(node, completed[index:]); consumed > 0 {
			index += consumed
			continue
		}
		child := node.children[word]
		if child == nil {
			index++
			continue
		}
		node = child
		index++
	}

	var candidates []string
	if strings.HasPrefix(current, "-") {
		candidates = app.flagCandidates(node)
	} else {
		for segment := range node.children {
			candidates = append(candidates, segment)
		}
		if node == app.root {
			candidates = append(candidates, "help")
			if app.completion {
				candidates = append(candidates, "completion")
			}
		}
	}

	set := make(map[string]struct{})
	filtered := candidates[:0]
	for _, candidate := range candidates {
		if !strings.HasPrefix(candidate, current) {
			continue
		}
		if _, exists := set[candidate]; exists {
			continue
		}
		set[candidate] = struct{}{}
		filtered = append(filtered, candidate)
	}
	sort.Strings(filtered)
	return filtered
}

// completionFlagWidth reports how many completed words belong to a known flag.
func (app *App) completionFlagWidth(node *commandNode, args []string) int {
	if len(args) == 0 || !strings.HasPrefix(args[0], "-") {
		return 0
	}
	lookup := func(name string) *pflag.Flag {
		if node.command != nil {
			if flag := node.command.flags.Lookup(name); flag != nil {
				return flag
			}
		}
		if app.globals != nil {
			return app.globals.Lookup(name)
		}
		return nil
	}
	word := args[0]
	if strings.HasPrefix(word, "--") {
		parts := strings.SplitN(strings.TrimPrefix(word, "--"), "=", 2)
		flag := lookup(parts[0])
		if flag == nil {
			return 0
		}
		if len(parts) == 2 || flag.NoOptDefVal != "" || len(args) == 1 {
			return 1
		}
		return 2
	}
	return 1
}

// completionIdentifier converts an executable name into a shell function name.
func completionIdentifier(name string) string {
	var builder strings.Builder
	for _, character := range name {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || character == '_' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

// shellQuote safely embeds a literal executable name in generated scripts.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
