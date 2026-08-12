package lockfile

import (
	"fmt"
	"strings"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/project"
)

// ValidateCurrent confirms that a complete lock describes exactly the resolved
// desired state. A matching pin is required because a pin is a trust limit.
func ValidateCurrent(resolved []manifest.ResolvedAsset, lock Lockfile) error {
	if err := Validate(lock); err != nil {
		return err
	}
	if len(resolved) != len(lock.Files) {
		return staleError("")
	}
	for _, file := range resolved {
		if err := validateEntry(file, lock.Files[file.Name]); err != nil {
			return staleError(file.Name)
		}
	}
	return nil
}

// ValidateRetained ensures entries outside a targeted lock remain current.
// Entries selected for replacement are intentionally excluded from this check.
func ValidateRetained(resolved []manifest.ResolvedAsset, lock Lockfile, replacing map[string]bool) error {
	for _, file := range resolved {
		if replacing[file.Name] {
			continue
		}
		locked, exists := lock.Files[file.Name]
		if !exists || validateEntry(file, locked) != nil {
			return staleError(file.Name)
		}
	}
	return nil
}

func validateEntry(file manifest.ResolvedAsset, locked Asset) error {
	if locked.ResolvedURL != file.ResolvedURL || locked.ResolvedFile != file.ResolvedFile {
		return ErrResolutionChanged
	}
	if file.Pin != "" {
		pin, _ := asset.NormalizeDigest(file.Pin)
		digest, _ := asset.NormalizeDigest(locked.Digest)
		if pin != digest {
			return ErrPinChanged
		}
	}
	return nil
}

func staleError(name string) error {
	return project.NewConfigurationError(ErrStale, project.WithAsset(name), project.WithHint(Hint(name)))
}

// Hint formats a command that safely selects one opaque asset name.
func Hint(name string) string {
	if name == "" {
		return "run `dac lock`"
	}
	return fmt.Sprintf("run: dac lock -- %s", shellQuote(name))
}

// shellQuote returns one POSIX shell word that reproduces value exactly.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
