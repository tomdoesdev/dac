package manifest

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"github.com/tomdoesdev/dac/internal/asset"
)

const initialExamples = `
# Project-wide variables are referenced as {{$.KEY}} by any file.
# [globals]
# VERSION = "1.2.3"

# Each download is declared under a unique name in [files].
# [files.example]
# url = "https://example.com/releases/{{$.VERSION}}/{{.PLATFORM}}/artifact.tar.gz"
# file = "artifact-{{.PLATFORM}}.tar.gz"
# pin = "sha256:<64 hexadecimal characters>" # Optional expected SHA-256 digest.
# max_size = "4GiB"                          # Use "0" to disable the size limit.
# idle_timeout = "30s"                       # Use "0" to disable the idle timeout.

# File-specific variables are referenced as {{.KEY}} by that file only.
# [files.example.variables]
# PLATFORM = "linux-amd64"

# Header values can read secrets from the environment at request time.
# [files.example.headers]
# Authorization = "env:ARTIFACT_TOKEN"
`

// encodeInitial adds a disposable reference block to a newly initialized
// manifest. Canonical rewrites intentionally omit it because comments are not
// part of the manifest's structured state.
func encodeInitial(value Manifest) []byte {
	return append(encode(value), initialExamples...)
}

func encode(value Manifest) []byte {
	var result strings.Builder
	fmt.Fprintf(&result, "version = %d\n", value.Version)
	// Globals precede the file tables because TOML binds every following
	// key to the table it was written under.
	if len(value.Globals) > 0 {
		result.WriteString("\n[globals]\n")
		for _, key := range sortedKeys(value.Globals) {
			fmt.Fprintf(&result, "%s = %s\n", tomlQuote(key), tomlQuote(value.Globals[key]))
		}
	}
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
