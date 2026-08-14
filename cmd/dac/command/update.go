package command

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/output"
)

type updateCommand struct {
	runtime *runtime
	Name    string   `arg:"name"`
	Set     []string `flag:"set" help:"set release variables (KEY=VALUE or $.KEY=VALUE)"`

	sets scopedVariableAssignments
}

func newUpdateCommand(runtime *runtime) *updateCommand { return &updateCommand{runtime: runtime} }

func (*updateCommand) Description() string { return "Set existing or referenced release variables" }

func (command *updateCommand) Validate() error {
	if err := command.runtime.Output.ValidateOptions(); err != nil {
		return err
	}
	sets, err := parseSets(command.Set)
	if err != nil {
		return err
	}
	if len(command.Set) == 0 {
		return ErrNoUpdate
	}
	if command.Name == "" {
		if len(sets.asset) > 0 {
			return ErrProjectVariableScope
		}
	} else if len(sets.globals) > 0 {
		return ErrAssetVariableScope
	}
	command.sets = sets
	return nil
}

func (command *updateCommand) Run(ctx context.Context) error {
	paths, err := command.runtime.project()
	if err != nil {
		return err
	}
	return paths.WithLock(ctx, func(context.Context) error {
		value, err := manifest.Load(paths.Manifest())
		if err != nil {
			return err
		}
		result, err := command.apply(&value)
		if err != nil {
			return err
		}
		if err := manifest.ValidateAvailable(value); err != nil {
			return err
		}
		if err := manifest.Write(paths.Manifest(), value); err != nil {
			return err
		}
		return command.runtime.Output.Success("update", paths.Root, []output.Result{result}, nil)
	})
}

// apply selects exactly one variable scope and mutates only the in-memory
// candidate, leaving validation and the atomic manifest write to Run.
func (command *updateCommand) apply(value *manifest.Manifest) (output.Result, error) {
	if command.Name == "" {
		referenced, err := referencedGlobals(*value)
		if err != nil {
			return output.Result{}, err
		}
		updated, changed, err := setVariables(maps.Clone(value.Globals), command.sets.globals, referenced, "global variable")
		if err != nil {
			return output.Result{}, fault.NewConfigurationError(err)
		}
		if !changed {
			return output.Result{}, fault.NewConfigurationError(ErrNoUpdate)
		}
		value.Globals = updated
		return output.Result{Name: "globals", Status: "updated"}, nil
	}

	file, exists := value.Files[command.Name]
	if !exists {
		return output.Result{}, fault.NewConfigurationError(ErrAssetNotFound, fault.WithAsset(command.Name))
	}
	references, err := manifest.ReferencedVariables(file)
	if err != nil {
		return output.Result{}, fault.NewConfigurationError(err, fault.WithAsset(command.Name))
	}
	updated, changed, err := setVariables(maps.Clone(file.Variables), command.sets.asset, references.Local, "asset variable")
	if err != nil {
		return output.Result{}, fault.NewConfigurationError(err, fault.WithAsset(command.Name))
	}
	if !changed {
		return output.Result{}, fault.NewConfigurationError(ErrNoUpdate, fault.WithAsset(command.Name))
	}
	file.Variables = updated
	value.Files[command.Name] = file
	return output.Result{Name: command.Name, Status: "updated", File: file.File}, nil
}

// setVariables updates assigned values and materializes only keys declared by
// template references. All preconditions are checked before the clone changes.
func setVariables(current, requested map[string]string, referenced map[string]struct{}, scope string) (map[string]string, bool, error) {
	for _, key := range slices.Sorted(maps.Keys(requested)) {
		if _, exists := current[key]; exists {
			continue
		}
		if _, declared := referenced[key]; !declared {
			return nil, false, fmt.Errorf("%w: %s %q", ErrVariableNotFound, scope, key)
		}
	}
	changed := false
	if current == nil {
		current = make(map[string]string, len(requested))
	}
	for key, value := range requested {
		if existing, exists := current[key]; !exists || existing != value {
			changed = true
		}
		current[key] = value
	}
	return current, changed, nil
}

// referencedGlobals finds declarations across the complete desired state so a
// first project assignment is accepted only when an asset actually uses it.
func referencedGlobals(value manifest.Manifest) (map[string]struct{}, error) {
	result := make(map[string]struct{})
	for _, file := range value.Files {
		references, err := manifest.ReferencedVariables(file)
		if err != nil {
			return nil, err
		}
		for name := range references.Global {
			result[name] = struct{}{}
		}
	}
	return result, nil
}
