// Package secret owns dac's indirection for configuration values a manifest
// must not contain literally. A header value written as "env:NAME" names an
// environment variable instead of carrying the credential itself, which keeps
// the secret out of dac.toml, dac.lock, and version control.
package secret

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Prefix marks a value as a reference rather than a literal.
const Prefix = "env:"

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var (
	// ErrInvalidReference marks a reference that names no usable variable.
	ErrInvalidReference = errors.New("environment variable reference is invalid")
	// ErrMissingValue marks a reference whose variable is not set.
	ErrMissingValue = errors.New("missing HTTP header environment variable")
)

// IsReference reports whether value defers to the environment.
func IsReference(value string) bool { return strings.HasPrefix(value, Prefix) }

// Validate checks a reference's grammar without reading the environment, so a
// manifest can be validated on a machine where the secret is absent.
func Validate(value string) error {
	name, found := strings.CutPrefix(value, Prefix)
	if !found {
		return nil
	}
	if !namePattern.MatchString(name) {
		return ErrInvalidReference
	}
	return nil
}

// Resolve returns the literal value a configured value stands for. Callers
// resolve at the last possible moment so a secret exists only for as long as
// the operation using it, and never enters persistent state or a diagnostic.
func Resolve(value string) (string, error) {
	name, found := strings.CutPrefix(value, Prefix)
	if !found {
		return value, nil
	}
	if !namePattern.MatchString(name) {
		return "", ErrInvalidReference
	}
	resolved, exists := os.LookupEnv(name)
	if !exists {
		return "", fmt.Errorf("%w: %s", ErrMissingValue, name)
	}
	return resolved, nil
}
