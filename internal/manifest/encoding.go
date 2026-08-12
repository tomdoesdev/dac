package manifest

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/tomdoesdev/dac/internal/asset"
)

func encode(value Manifest) []byte {
	var result strings.Builder
	fmt.Fprintf(&result, "version = %d\n", value.Version)
	for _, name := range sortedKeys(value.Files) {
		file := value.Files[name]
		fmt.Fprintf(&result, "\n[files.%s]\nurl = %s\nfile = %s\n", tomlQuote(name), tomlQuote(file.URL), tomlQuote(file.File))
		if file.Pin != "" {
			digest, _ := asset.NormalizeDigest(file.Pin)
			fmt.Fprintf(&result, "pin = %s\n", tomlQuote(digest))
		}
		if file.MaxSize != "" {
			fmt.Fprintf(&result, "max_size = %s\n", tomlQuote(file.MaxSize))
		}
		if file.IdleTimeout != "" {
			fmt.Fprintf(&result, "idle_timeout = %s\n", tomlQuote(file.IdleTimeout))
		}
		if len(file.Variables) > 0 {
			fmt.Fprintf(&result, "\n[files.%s.variables]\n", tomlQuote(name))
			for _, key := range sortedKeys(file.Variables) {
				fmt.Fprintf(&result, "%s = %s\n", tomlQuote(key), tomlQuote(file.Variables[key]))
			}
		}
		if len(file.Headers) > 0 {
			fmt.Fprintf(&result, "\n[files.%s.headers]\n", tomlQuote(name))
			for _, key := range sortedKeys(file.Headers) {
				fmt.Fprintf(&result, "%s = %s\n", tomlQuote(key), tomlQuote(file.Headers[key]))
			}
		}
	}
	return []byte(result.String())
}

func tomlQuote(value string) string {
	// TOML's basic-string escape set is smaller than Go's; this form keeps
	// ordinary Unicode intact and never emits Go-only \x escapes.
	var result strings.Builder
	result.WriteByte('"')
	for _, character := range value {
		switch character {
		case '\\':
			result.WriteString(`\\`)
		case '"':
			result.WriteString(`\"`)
		case '\b':
			result.WriteString(`\b`)
		case '\t':
			result.WriteString(`\t`)
		case '\n':
			result.WriteString(`\n`)
		case '\f':
			result.WriteString(`\f`)
		case '\r':
			result.WriteString(`\r`)
		default:
			if unicode.IsControl(character) {
				if character <= 0xffff {
					fmt.Fprintf(&result, `\u%04X`, character)
				} else {
					fmt.Fprintf(&result, `\U%08X`, character)
				}
				continue
			}
			result.WriteRune(character)
		}
	}
	result.WriteByte('"')
	return result.String()
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
