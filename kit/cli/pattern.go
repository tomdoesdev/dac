package cli

import (
	"strings"
)

type commandPattern struct {
	path      []string
	arguments []patternArgument
}

type patternArgument struct {
	name     string
	optional bool
	variadic bool
}

func (argument patternArgument) syntax() string {
	name := argument.name
	if argument.variadic {
		name += "..."
	}

	if argument.optional {
		return "[" + name + "]"
	}

	return "<" + name + ">"
}

// parsePattern treats every token before the first positional declaration as a
// command literal. The grammar needs no sigil because command and argument
// tokens are already unambiguous.
func parsePattern(value string) (commandPattern, error) {
	tokens := strings.Fields(value)
	if len(tokens) == 0 {
		return commandPattern{}, newCLIError(ErrInvalidCommandDefinition, "cli: command pattern must not be empty")
	}

	pattern := commandPattern{}
	seenArgument := false
	seenOptional := false
	argumentNames := make(map[string]struct{})

	for index, token := range tokens {
		if token == "%flags%" || strings.Contains(token, "%") {
			return commandPattern{}, newCLIError(ErrInvalidCommandDefinition, "cli: %%flags%% is not supported; tagged flags are parsed automatically")
		}

		if token[0] != '<' && token[0] != '[' {
			if seenArgument {
				return commandPattern{}, newCLIError(ErrInvalidCommandDefinition, "cli: command literal %q must appear before positional arguments", token)
			}
			if !isPatternName(token) {
				return commandPattern{}, newCLIError(ErrInvalidCommandDefinition, "cli: invalid command literal %q", token)
			}
			if len(pattern.path) == 0 && isReservedCommand(token) {
				return commandPattern{}, newCLIError(ErrInvalidCommandDefinition, "cli: top-level command %q is reserved", token)
			}

			pattern.path = append(pattern.path, token)
			continue
		}

		argument, err := parsePatternArgument(token)
		if err != nil {
			return commandPattern{}, err
		}
		seenArgument = true

		if argument.variadic && index != len(tokens)-1 {
			return commandPattern{}, newCLIError(ErrInvalidCommandDefinition, "cli: variadic argument %q must be last", argument.name)
		}
		if !argument.optional && seenOptional {
			return commandPattern{}, newCLIError(ErrInvalidCommandDefinition, "cli: required argument %q cannot follow an optional argument", argument.name)
		}
		if argument.optional {
			seenOptional = true
		}
		if _, exists := argumentNames[argument.name]; exists {
			return commandPattern{}, newCLIError(ErrInvalidCommandDefinition, "cli: positional argument %q is declared more than once", argument.name)
		}

		argumentNames[argument.name] = struct{}{}
		pattern.arguments = append(pattern.arguments, argument)
	}

	if len(pattern.path) == 0 {
		return commandPattern{}, newCLIError(ErrInvalidCommandDefinition, "cli: command pattern must declare at least one command literal")
	}

	return pattern, nil
}

func parsePatternArgument(token string) (patternArgument, error) {
	if len(token) < 3 {
		return patternArgument{}, newCLIError(ErrInvalidCommandDefinition, "cli: invalid positional argument %q", token)
	}

	optional := token[0] == '['
	required := token[0] == '<'
	if !optional && !required {
		return patternArgument{}, newCLIError(ErrInvalidCommandDefinition, "cli: expected command, <argument>, or [argument], got %q", token)
	}

	closing := byte('>')
	if optional {
		closing = ']'
	}
	if token[len(token)-1] != closing {
		return patternArgument{}, newCLIError(ErrInvalidCommandDefinition, "cli: invalid positional argument %q", token)
	}

	name := token[1 : len(token)-1]
	variadic := strings.HasSuffix(name, "...")
	if variadic {
		name = strings.TrimSuffix(name, "...")
	}
	if strings.Contains(name, "...") || !isPatternName(name) {
		return patternArgument{}, newCLIError(ErrInvalidCommandDefinition, "cli: invalid positional argument %q", token)
	}

	return patternArgument{name: name, optional: optional, variadic: variadic}, nil
}

func isPatternName(value string) bool {
	if value == "" || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}

	for _, character := range value {
		if character == '-' || character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' {
			continue
		}
		return false
	}

	return true
}

// parseCommandPath parses aliases and group paths, which intentionally cannot
// declare positional arguments.
func parseCommandPath(value string) ([]string, error) {
	pattern, err := parsePattern(value)
	if err != nil {
		return nil, err
	}
	if len(pattern.arguments) != 0 {
		return nil, newCLIError(ErrInvalidCommandDefinition, "cli: command path %q must not declare positional arguments", value)
	}

	return pattern.path, nil
}

// isReservedCommand protects built-in command paths whether or not an optional
// feature is enabled, so enabling it later cannot silently shadow user code.
func isReservedCommand(value string) bool {
	return value == "help" || value == "completion" || value == "__complete"
}
