package manifest

import (
	"fmt"
	"strings"
)

type variableScope uint8

const (
	localVariable variableScope = iota
	globalVariable
)

type templatePart struct {
	literal string
	name    string
	scope   variableScope
}

// VariableReferences is the set of local and project variables read by one
// asset. Commands use it to distinguish a declared-but-unset value from a typo.
type VariableReferences struct {
	Local  map[string]struct{}
	Global map[string]struct{}
}

type compiledTemplate struct {
	kind       string
	parts      []templatePart
	references VariableReferences
}

// compileTemplate accepts only direct substitutions. Keeping parsing and
// reference discovery together prevents unresolved configuration from being
// interpreted differently by add, update, and pull.
func compileTemplate(kind, value string) (compiledTemplate, error) {
	result := compiledTemplate{
		kind: kind,
		references: VariableReferences{
			Local:  make(map[string]struct{}),
			Global: make(map[string]struct{}),
		},
	}
	for len(value) > 0 {
		opening := strings.Index(value, "{{")
		closing := strings.Index(value, "}}")
		if closing >= 0 && (opening < 0 || closing < opening) {
			return compiledTemplate{}, fmt.Errorf("%w in %s", ErrInvalidTemplate, kind)
		}
		if opening < 0 {
			result.parts = append(result.parts, templatePart{literal: value})
			break
		}
		if opening > 0 {
			result.parts = append(result.parts, templatePart{literal: value[:opening]})
		}
		value = value[opening+2:]
		closing = strings.Index(value, "}}")
		if closing < 0 || strings.Contains(value[:closing], "{{") {
			return compiledTemplate{}, fmt.Errorf("%w in %s", ErrInvalidTemplate, kind)
		}
		expression := strings.TrimSpace(value[:closing])
		part := templatePart{}
		switch {
		case strings.HasPrefix(expression, "$."):
			part.scope = globalVariable
			part.name = strings.TrimPrefix(expression, "$.")
		case strings.HasPrefix(expression, "."):
			part.scope = localVariable
			part.name = strings.TrimPrefix(expression, ".")
		default:
			return compiledTemplate{}, fmt.Errorf("%w in %s", ErrInvalidTemplate, kind)
		}
		if !ValidVariableName(part.name) {
			return compiledTemplate{}, fmt.Errorf("%w in %s: %q", ErrInvalidTemplate, kind, expression)
		}
		if part.scope == globalVariable {
			result.references.Global[part.name] = struct{}{}
		} else {
			result.references.Local[part.name] = struct{}{}
		}
		result.parts = append(result.parts, part)
		value = value[closing+2:]
	}
	return result, nil
}

// ReferencedVariables returns every value an asset's URL and filename read.
func ReferencedVariables(file Asset) (VariableReferences, error) {
	result := VariableReferences{Local: make(map[string]struct{}), Global: make(map[string]struct{})}
	for _, input := range []struct {
		kind  string
		value string
	}{{kind: "url", value: file.URL}, {kind: "file", value: file.File}} {
		compiled, err := compileTemplate(input.kind, input.value)
		if err != nil {
			return VariableReferences{}, err
		}
		for name := range compiled.references.Local {
			result.Local[name] = struct{}{}
		}
		for name := range compiled.references.Global {
			result.Global[name] = struct{}{}
		}
	}
	return result, nil
}

func (compiled compiledTemplate) render(globals, locals map[string]string) (string, error) {
	var result strings.Builder
	for _, part := range compiled.parts {
		if part.name == "" {
			result.WriteString(part.literal)
			continue
		}
		values, scope := locals, "local"
		if part.scope == globalVariable {
			values, scope = globals, "global"
		}
		value, exists := values[part.name]
		if !exists {
			return "", fmt.Errorf("%w in %s: %s variable %q", ErrVariableUnset, compiled.kind, scope, part.name)
		}
		result.WriteString(value)
	}
	return result.String(), nil
}

func (compiled compiledTemplate) canRender(globals, locals map[string]string) bool {
	for name := range compiled.references.Global {
		if _, exists := globals[name]; !exists {
			return false
		}
	}
	for name := range compiled.references.Local {
		if _, exists := locals[name]; !exists {
			return false
		}
	}
	return true
}

// renderProvisional substitutes a URL-safe value for every reference so
// validation can reject unsafe static structure before real values exist.
func (compiled compiledTemplate) renderProvisional() string {
	values := make(map[string]string, len(compiled.references.Local)+len(compiled.references.Global))
	for name := range compiled.references.Local {
		values[name] = "value"
	}
	for name := range compiled.references.Global {
		values[name] = "value"
	}
	result, _ := compiled.render(values, values)
	return result
}
