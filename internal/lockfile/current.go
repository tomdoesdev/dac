package lockfile

import (
	"sort"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/manifest"
)

// CurrentState is the manifest-to-lock state used by pull.
type CurrentState string

const (
	StateCurrent CurrentState = "current"
	StateStale   CurrentState = "stale"
)

// EntryState describes one manifest asset and its optional lock entry.
type EntryState struct {
	Resolved manifest.ResolvedAsset
	Locked   Asset
	State    CurrentState
}

// Evaluation describes every manifest asset plus lock-only orphan entries.
type Evaluation struct {
	Entries []EntryState
	Orphans []string
}

// Evaluate computes lock currentness once so all workflows enforce identical
// configuration, resolution, and pin policy.
func Evaluate(resolved manifest.Resolution, lock Lockfile) (Evaluation, error) {
	if err := Validate(lock); err != nil {
		return Evaluation{}, err
	}
	result := Evaluation{Entries: make([]EntryState, 0, resolved.Len())}
	for _, file := range resolved.All() {
		locked, exists := lock.Files[file.Name]
		entry := EntryState{Resolved: file, Locked: locked, State: StateCurrent}
		if !exists {
			entry.State = StateStale
		} else if err := validateEntry(file, locked); err != nil {
			entry.State = StateStale
		}
		result.Entries = append(result.Entries, entry)
	}
	for name := range lock.Files {
		if !resolved.Has(name) {
			result.Orphans = append(result.Orphans, name)
		}
	}
	sort.Strings(result.Orphans)
	return result, nil
}

// ValidateCurrent confirms that a complete lock describes exactly the resolved
// desired state. A matching pin is required because a pin is a trust limit.
func ValidateCurrent(resolved manifest.Resolution, lock Lockfile) error {
	evaluation, err := Evaluate(resolved, lock)
	if err != nil {
		return err
	}
	if len(evaluation.Orphans) > 0 {
		return staleError("")
	}
	for _, entry := range evaluation.Entries {
		if entry.State != StateCurrent {
			return staleError("")
		}
	}
	return nil
}

// ValidateSelected confirms that selected entries are current. A complete
// selection additionally owns orphan cleanup; a named scope deliberately
// ignores lock state outside the assets the caller requested.
func ValidateSelected(evaluation Evaluation, selected map[string]bool, complete bool) error {
	if complete {
		if len(evaluation.Orphans) > 0 {
			return staleError("")
		}
		for _, entry := range evaluation.Entries {
			if entry.State != StateCurrent {
				return staleError("")
			}
		}
		return nil
	}
	for _, entry := range evaluation.Entries {
		if !selected[entry.Resolved.Name] {
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
	recovery := UpdateRecovery()
	if name != "" {
		recovery = UpdateRecovery(name)
	}
	return fault.NewConfigurationError(ErrStale, fault.WithAsset(name), fault.WithRecovery(recovery))
}

// UpdateRecovery selects the narrowest pull that can refresh accepted state.
// Asset names stay structured data so the output boundary can quote them.
func UpdateRecovery(names ...string) fault.Recovery {
	return fault.Recovery{Command: "pull", Flags: []string{"--update-lockfile"}, Assets: names}
}
