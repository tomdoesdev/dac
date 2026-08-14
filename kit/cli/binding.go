package cli

import (
	"encoding"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

var durationType = reflect.TypeOf(time.Duration(0))

var textUnmarshalerType = reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()

// boolFlagValue is the convention pflag uses for values whose option can be
// supplied without an explicit argument. Keeping the check here lets custom
// domain values opt into that useful command-line form.
type boolFlagValue interface {
	IsBoolFlag() bool
}

const shortOnlyFlagPrefix = "\ue000"

type command struct {
	path         []string
	handler      CommandHandler
	handlerValue reflect.Value
	description  string
	help         string
	arguments    []argumentBinding
	flags        *pflag.FlagSet
	flagCount    int
	aliases      [][]string
	deprecated   string

	helpRequested bool
}

type argumentBinding struct {
	patternArgument
	fieldIndex int
	help       string
}

type flagBinding struct {
	fieldIndex int
	name       string
	shorthand  string
	help       string
}

// newCommand validates a handler once and constructs a private FlagSet that
// writes nowhere. The caller owns rendering errors via UsageError instead.
func newCommand(patternValue string, handler CommandHandler, options ...CommandOption) (*command, error) {
	pattern, err := parsePattern(patternValue)
	if err != nil {
		return nil, err
	}

	value := reflect.ValueOf(handler)
	if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() || value.Elem().Kind() != reflect.Struct {
		return nil, newCLIError(ErrInvalidCommandDefinition, "cli: command handler must be a non-nil pointer to a struct")
	}

	description := strings.TrimSpace(handler.Description())
	if description == "" {
		return nil, newCLIError(ErrInvalidCommandDefinition, "cli: command %q must provide a non-empty Description", strings.Join(pattern.path, " "))
	}

	config := commandConfig{}
	for _, option := range options {
		if option == nil {
			return nil, newCLIError(ErrInvalidCommandDefinition, "cli: command option must not be nil")
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}

	command := &command{
		path:         append([]string(nil), pattern.path...),
		handler:      handler,
		handlerValue: value.Elem(),
		description:  description,
		deprecated:   config.deprecated,
	}
	if provider, ok := handler.(HelpProvider); ok {
		command.help = strings.TrimSpace(provider.Help())
	}
	for _, aliasValue := range config.aliases {
		alias, err := parseCommandPath(aliasValue)
		if err != nil {
			return nil, wrapCLIError(ErrInvalidCommandDefinition, err, "cli: invalid alias %q: %v", aliasValue, err)
		}
		command.aliases = append(command.aliases, alias)
	}

	argumentFields, flags, err := inspectHandlerFields(command.handlerValue.Type())
	if err != nil {
		return nil, err
	}

	for _, argument := range pattern.arguments {
		fieldIndex, exists := argumentFields[argument.name]
		if !exists {
			return nil, newCLIError(ErrInvalidCommandDefinition, "cli: positional argument %q has no matching arg-tagged field", argument.name)
		}

		field := command.handlerValue.Type().Field(fieldIndex)
		if err := validateArgumentField(field, command.handlerValue.Field(fieldIndex), argument); err != nil {
			return nil, err
		}

		command.arguments = append(command.arguments, argumentBinding{
			patternArgument: argument,
			fieldIndex:      fieldIndex,
			help:            field.Tag.Get("help"),
		})
	}

	for name := range argumentFields {
		if !hasPatternArgument(pattern.arguments, name) {
			return nil, newCLIError(ErrInvalidCommandDefinition, "cli: arg-tagged field %q does not appear in command pattern", name)
		}
	}

	command.flags = pflag.NewFlagSet(strings.Join(command.path, " "), pflag.ContinueOnError)
	command.flags.SetInterspersed(true)
	command.flags.SetOutput(io.Discard)
	for _, binding := range flags {
		if err := command.addFlag(binding); err != nil {
			return nil, err
		}
	}
	command.flagCount = len(flags)
	command.flags.BoolVarP(&command.helpRequested, "help", "h", false, "show help")

	return command, nil
}

func inspectHandlerFields(handlerType reflect.Type) (map[string]int, []flagBinding, error) {
	// Only direct fields participate. Flattening embedded structs would make
	// tag collisions implicit and make command definitions harder to inspect.
	arguments := make(map[string]int)
	var flags []flagBinding
	flagNames := make(map[string]struct{})
	shorthands := make(map[string]struct{})

	for index := 0; index < handlerType.NumField(); index++ {
		field := handlerType.Field(index)
		argumentName, hasArgument := field.Tag.Lookup("arg")
		flagValue, hasFlag := field.Tag.Lookup("flag")
		if !hasArgument && !hasFlag {
			continue
		}

		if field.PkgPath != "" {
			return nil, nil, newCLIError(ErrInvalidCommandDefinition, "cli: tagged field %s must be exported", field.Name)
		}
		if hasArgument && hasFlag {
			return nil, nil, newCLIError(ErrInvalidCommandDefinition, "cli: field %s cannot have both arg and flag tags", field.Name)
		}

		if hasArgument {
			if !isPatternName(argumentName) {
				return nil, nil, newCLIError(ErrInvalidCommandDefinition, "cli: field %s has invalid arg tag %q", field.Name, argumentName)
			}
			if _, exists := arguments[argumentName]; exists {
				return nil, nil, newCLIError(ErrInvalidCommandDefinition, "cli: arg tag %q is used more than once", argumentName)
			}

			arguments[argumentName] = index
			continue
		}

		name, shorthand, err := parseFlagTag(flagValue)
		if err != nil {
			return nil, nil, wrapCLIError(ErrInvalidCommandDefinition, err, "cli: field %s: %v", field.Name, err)
		}
		if name == "help" || shorthand == "h" {
			return nil, nil, newCLIError(ErrInvalidCommandDefinition, "cli: field %s conflicts with built-in help flag", field.Name)
		}
		if name != "" {
			if _, exists := flagNames[name]; exists {
				return nil, nil, newCLIError(ErrInvalidCommandDefinition, "cli: flag %q is used more than once", name)
			}
			flagNames[name] = struct{}{}
		}
		if shorthand != "" {
			if _, exists := shorthands[shorthand]; exists {
				return nil, nil, newCLIError(ErrInvalidCommandDefinition, "cli: shorthand %q is used more than once", shorthand)
			}
			shorthands[shorthand] = struct{}{}
		}

		flags = append(flags, flagBinding{
			fieldIndex: index,
			name:       name,
			shorthand:  shorthand,
			help:       field.Tag.Get("help"),
		})
	}

	return arguments, flags, nil
}

func parseFlagTag(value string) (string, string, error) {
	// Keep the tag grammar deliberately shallow: the long name and optional
	// shorthand are all pflag needs, while "-" explicitly omits the long name.
	parts := strings.Split(value, ",")
	if len(parts) > 2 {
		return "", "", newCLIError(ErrInvalidCommandDefinition, "flag tag %q must be long-name or long-name,short", value)
	}

	name := strings.TrimSpace(parts[0])
	shortOnly := name == "-"
	if !shortOnly && !isFlagName(name) {
		return "", "", newCLIError(ErrInvalidCommandDefinition, "invalid flag name %q", name)
	}
	if shortOnly {
		name = ""
	}

	shorthand := ""
	if len(parts) == 2 {
		shorthand = strings.TrimSpace(parts[1])
		if !isFlagShorthand(shorthand) {
			return "", "", newCLIError(ErrInvalidCommandDefinition, "invalid shorthand %q", shorthand)
		}
	}
	if shortOnly && shorthand == "" {
		return "", "", newCLIError(ErrInvalidCommandDefinition, "short-only flag must provide a shorthand")
	}

	return name, shorthand, nil
}

func isFlagName(value string) bool {
	return isPatternName(value)
}

// isFlagShorthand limits short options to one ASCII letter, avoiding pflag
// punctuation forms that are difficult to address consistently from shells.
func isFlagShorthand(value string) bool {
	return len(value) == 1 && (value[0] >= 'A' && value[0] <= 'Z' || value[0] >= 'a' && value[0] <= 'z')
}

func validateArgumentField(field reflect.StructField, value reflect.Value, argument patternArgument) error {
	if argument.variadic {
		if field.Type.Kind() != reflect.Slice {
			return newCLIError(ErrInvalidCommandDefinition, "cli: variadic argument %q requires a slice field", argument.name)
		}
		if _, _, err := flagValue(reflect.New(field.Type.Elem()).Elem()); err != nil {
			return newCLIError(ErrInvalidCommandDefinition, "cli: variadic argument %q has unsupported element type %s", argument.name, field.Type.Elem())
		}

		return nil
	}

	if _, _, err := flagValue(value); err != nil {
		return wrapCLIError(ErrInvalidCommandDefinition, err, "cli: positional argument %q: %v", argument.name, err)
	}

	return nil
}

func hasPatternArgument(arguments []patternArgument, name string) bool {
	for _, argument := range arguments {
		if argument.name == name {
			return true
		}
	}

	return false
}

func (command *command) addFlag(binding flagBinding) error {
	return addFlag(command.flags, command.handlerValue.Field(binding.fieldIndex), binding)
}

// addFlag binds one reflected field to a FlagSet and preserves bool flags'
// value-optional command-line behavior.
func addFlag(flags *pflag.FlagSet, field reflect.Value, binding flagBinding) error {
	value, isBool, err := flagValue(field)
	if err != nil {
		return wrapCLIError(ErrInvalidCommandDefinition, err, "cli: flag %q: %v", binding.name, err)
	}

	name := binding.name
	if name == "" {
		// pflag requires every shorthand to have a map key. A private-use prefix
		// is outside this package's long-name grammar, so the synthetic key is
		// kept out of documented and completed command-line spellings.
		name = shortOnlyFlagPrefix + binding.shorthand
	}
	flags.VarP(value, name, binding.shorthand, binding.help)
	if isBool {
		flags.Lookup(name).NoOptDefVal = "true"
	}

	return nil
}

// isShortOnlyFlag identifies the private pflag registration used for tags
// whose long-name slot is "-".
func isShortOnlyFlag(flag *pflag.Flag) bool {
	return flag != nil && strings.HasPrefix(flag.Name, shortOnlyFlagPrefix)
}

func (command *command) bindArguments(values []string) error {
	// Validate arity before changing fields so Validate never observes a
	// partially bound command after a malformed invocation.
	minimum := 0
	variadic := false
	for _, argument := range command.arguments {
		if !argument.optional {
			minimum++
		}
		if argument.variadic {
			variadic = true
		}
	}

	if len(values) < minimum {
		requiredSeen := 0
		for _, argument := range command.arguments {
			if argument.optional {
				continue
			}
			if requiredSeen == len(values) {
				return newCLIError(ErrInvalidInvocation, "missing required positional argument <%s>", argument.name)
			}
			requiredSeen++
		}
	}
	if !variadic && len(values) > len(command.arguments) {
		return newCLIError(ErrInvalidInvocation, "unexpected positional argument %q", values[len(command.arguments)])
	}

	valueIndex := 0
	for _, argument := range command.arguments {
		field := command.handlerValue.Field(argument.fieldIndex)
		if argument.variadic {
			field.Set(reflect.MakeSlice(field.Type(), len(values)-valueIndex, len(values)-valueIndex))
			for index, input := range values[valueIndex:] {
				if err := setFieldValue(field.Index(index), input); err != nil {
					return wrapCLIError(ErrInvalidInvocation, err, "invalid value for <%s>: %v", argument.name, err)
				}
			}
			return nil
		}

		if valueIndex == len(values) {
			field.SetZero()
			continue
		}

		if err := setFieldValue(field, values[valueIndex]); err != nil {
			return wrapCLIError(ErrInvalidInvocation, err, "invalid value for <%s>: %v", argument.name, err)
		}
		valueIndex++
	}

	return nil
}

// setFieldValue uses the same conversion contract for flags and positional
// arguments, keeping custom domain types consistent across both input forms.
func setFieldValue(field reflect.Value, input string) error {
	value, _, err := flagValue(field)
	if err != nil {
		return err
	}

	return value.Set(input)
}

func flagValue(field reflect.Value) (pflag.Value, bool, error) {
	// Prefer an explicit pflag.Value implementation before reflecting primitive
	// kinds; this gives domain types control over parsing without extra APIs.
	if field.CanInterface() {
		if value, ok := field.Interface().(pflag.Value); ok {
			if isNilValue(value) {
				return nil, false, newCLIError(ErrInvalidCommandDefinition, "pflag.Value field must not be nil")
			}
			return value, isOptionalValue(value), nil
		}
	}
	if field.CanAddr() && field.Addr().CanInterface() {
		if value, ok := field.Addr().Interface().(pflag.Value); ok {
			if isNilValue(value) {
				return nil, false, newCLIError(ErrInvalidCommandDefinition, "pflag.Value field must not be nil")
			}
			return value, isOptionalValue(value), nil
		}
	}
	if field.CanInterface() && field.Type().Implements(textUnmarshalerType) {
		return textValue{field: field}, false, nil
	}
	if field.CanAddr() && field.Addr().CanInterface() && field.Addr().Type().Implements(textUnmarshalerType) {
		return textValue{field: field}, false, nil
	}

	if field.Type() == durationType {
		return reflectedValue{field: field, kind: "duration"}, false, nil
	}

	switch field.Kind() {
	case reflect.String:
		return reflectedValue{field: field, kind: "string"}, false, nil
	case reflect.Bool:
		return reflectedValue{field: field, kind: "bool"}, true, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return reflectedValue{field: field, kind: field.Kind().String()}, false, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return reflectedValue{field: field, kind: field.Kind().String()}, false, nil
	case reflect.Float32, reflect.Float64:
		return reflectedValue{field: field, kind: field.Kind().String()}, false, nil
	case reflect.Slice:
		if field.Type().Elem().Kind() == reflect.String {
			return stringSliceValue{field: field}, false, nil
		}
	}

	return nil, false, newCLIError(ErrInvalidCommandDefinition, "unsupported field type %s", field.Type())
}

// isOptionalValue recognizes pflag's standard optional-value extension
// without coupling callers to a concrete pflag implementation.
func isOptionalValue(value pflag.Value) bool {
	optional, ok := value.(boolFlagValue)
	return ok && optional.IsBoolFlag()
}

type textValue struct {
	field reflect.Value
}

// String returns a stable representation for pflag's default-value help.
func (value textValue) String() string {
	if value.field.CanInterface() {
		return fmt.Sprint(value.field.Interface())
	}
	return ""
}

// Set delegates parsing to encoding.TextUnmarshaler so domain types do not
// need to depend on pflag solely to participate in CLI binding.
func (value textValue) Set(input string) error {
	if value.field.CanInterface() && value.field.Type().Implements(textUnmarshalerType) {
		return value.field.Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(input))
	}
	return value.field.Addr().Interface().(encoding.TextUnmarshaler).UnmarshalText([]byte(input))
}

// Type describes text-unmarshaled fields without leaking their reflect syntax
// into generated usage.
func (value textValue) Type() string {
	if name := value.field.Type().Name(); name != "" {
		return name
	}
	return "text"
}

func isNilValue(value pflag.Value) bool {
	// A typed nil can satisfy pflag.Value but will panic when pflag reads its
	// default value during registration.
	reflectValue := reflect.ValueOf(value)
	return reflectValue.Kind() == reflect.Pointer && reflectValue.IsNil()
}

type reflectedValue struct {
	field reflect.Value
	kind  string
}

func (value reflectedValue) String() string {
	if value.field.Type() == durationType {
		return time.Duration(value.field.Int()).String()
	}

	switch value.field.Kind() {
	case reflect.String:
		return value.field.String()
	case reflect.Bool:
		return strconv.FormatBool(value.field.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(value.field.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(value.field.Uint(), 10)
	case reflect.Float32, reflect.Float64:
		return strconv.FormatFloat(value.field.Float(), 'g', -1, value.field.Type().Bits())
	default:
		return ""
	}
}

func (value reflectedValue) Set(input string) error {
	if value.field.Type() == durationType {
		parsed, err := time.ParseDuration(input)
		if err != nil {
			return err
		}
		value.field.SetInt(int64(parsed))
		return nil
	}

	switch value.field.Kind() {
	case reflect.String:
		value.field.SetString(input)
		return nil
	case reflect.Bool:
		parsed, err := strconv.ParseBool(input)
		if err != nil {
			return err
		}
		value.field.SetBool(parsed)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		parsed, err := strconv.ParseInt(input, 0, value.field.Type().Bits())
		if err != nil {
			return err
		}
		value.field.SetInt(parsed)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		parsed, err := strconv.ParseUint(input, 0, value.field.Type().Bits())
		if err != nil {
			return err
		}
		value.field.SetUint(parsed)
		return nil
	case reflect.Float32, reflect.Float64:
		parsed, err := strconv.ParseFloat(input, value.field.Type().Bits())
		if err != nil {
			return err
		}
		value.field.SetFloat(parsed)
		return nil
	default:
		return newCLIError(ErrInvalidCommandDefinition, "unsupported field type %s", value.field.Type())
	}
}

// Type describes the reflected value to pflag for generated help output.
func (value reflectedValue) Type() string {
	return value.kind
}

type stringSliceValue struct {
	field reflect.Value
}

func (value stringSliceValue) String() string {
	parts := make([]string, value.field.Len())
	for index := range parts {
		parts[index] = value.field.Index(index).String()
	}

	return strings.Join(parts, ",")
}

func (value stringSliceValue) Set(input string) error {
	element := reflect.New(value.field.Type().Elem()).Elem()
	element.SetString(input)
	value.field.Set(reflect.Append(value.field, element))
	return nil
}

// Type reports stringArray so pflag's usage text describes repeatable strings.
func (value stringSliceValue) Type() string {
	return "stringArray"
}
