package lockfile

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/project"
)

// CurrentState is the manifest-to-lock state used by pull, lock, and status.
type CurrentState string

const (
	StateCurrent CurrentState = "current"
	StateStale   CurrentState = "stale"
)

// EntryState describes one manifest asset and its optional lock entry.
type EntryState struct {
	Resolved manifest.ResolvedAsset
	Locked   Asset
	Exists   bool
	State    CurrentState
	Reason   string
}

// Evaluation describes every manifest asset plus lock-only orphan entries.
type Evaluation struct {
	Entries []EntryState
	Orphans []string
}

// Evaluate computes lock currentness once so all workflows enforce identical
// configuration, resolution, and pin policy.
func Evaluate(resolved []manifest.ResolvedAsset, lock Lockfile) (Evaluation, error) {
	if err := Validate(lock); err != nil {
		return Evaluation{}, err
	}
	result := Evaluation{Entries: make([]EntryState, 0, len(resolved))}
	manifestNames := make(map[string]bool, len(resolved))
	for _, file := range resolved {
		manifestNames[file.Name] = true
		locked, exists := lock.Files[file.Name]
		entry := EntryState{Resolved: file, Locked: locked, Exists: exists, State: StateCurrent}
		if !exists {
			entry.State, entry.Reason = StateStale, "asset is not locked"
		} else if err := validateEntry(file, locked); err != nil {
			entry.State, entry.Reason = StateStale, "source configuration changed"
		}
		result.Entries = append(result.Entries, entry)
	}
	for name := range lock.Files {
		if !manifestNames[name] {
			result.Orphans = append(result.Orphans, name)
		}
	}
	sort.Strings(result.Orphans)
	return result, nil
}

// ValidateCurrent confirms that a complete lock describes exactly the resolved
// desired state. A matching pin is required because a pin is a trust limit.
func ValidateCurrent(resolved []manifest.ResolvedAsset, lock Lockfile) error {
	evaluation, err := Evaluate(resolved, lock)
	if err != nil {
		return err
	}
	if len(evaluation.Orphans) > 0 {
		return staleError("")
	}
	for _, entry := range evaluation.Entries {
		if entry.State != StateCurrent {
			return staleError(entry.Resolved.Name)
		}
	}
	return nil
}

// ValidateRetained ensures entries outside a targeted lock remain current.
// Entries selected for replacement are intentionally excluded from this check.
func ValidateRetained(resolved []manifest.ResolvedAsset, lock Lockfile, replacing map[string]bool) error {
	evaluation, err := Evaluate(resolved, lock)
	if err != nil {
		return err
	}
	for _, entry := range evaluation.Entries {
		if replacing[entry.Resolved.Name] {
			continue
		}
		if entry.State != StateCurrent {
			return staleError(entry.Resolved.Name)
		}
	}
	return nil
}

// validateEntry applies the source and trust constraints that make one lock
// entry safe to retain without contacting its remote host again.
func validateEntry(file manifest.ResolvedAsset, locked Asset) error {
	if locked.ResolvedURL != file.ResolvedURL || locked.ResolvedFile != file.ResolvedFile {
		return ErrResolutionChanged
	}
	if locked.ConfigurationDigest != ConfigurationDigest(file) {
		return ErrConfigurationChanged
	}
	if file.Pin != "" {
		pin, _ := asset.NormalizeDigest(file.Pin)
		if pin != locked.Digest {
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
		return "run `dac lock --all`"
	}
	return fmt.Sprintf("run: dac lock -- %s", shellQuote(name))
}

// shellQuote returns one POSIX shell word that reproduces value exactly.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
