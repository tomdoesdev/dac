package command

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/output"
)

type updateCommand struct {
	runtime          *runtime
	Name             string       `arg:"name"`
	URL              *singleValue `flag:"url" help:"replace the URL template"`
	File             *singleValue `flag:"file" help:"replace the filename template"`
	Set              []string     `flag:"set" help:"set an artifact variable (KEY=VALUE)"`
	Unset            []string     `flag:"unset" help:"remove an artifact variable"`
	Header           []string     `flag:"header" help:"set an HTTP header (NAME=VALUE)"`
	RemoveHeader     []string     `flag:"remove-header" help:"remove an HTTP header"`
	Pin              pinValue     `flag:"pin" help:"calculate or require a SHA-256 pin"`
	Unpin            bool         `flag:"unpin" help:"remove the artifact pin"`
	MaxSize          *singleValue `flag:"max-size" help:"set the maximum response body size"`
	UnsetMaxSize     bool         `flag:"unset-max-size" help:"restore the default maximum response body size"`
	IdleTimeout      *singleValue `flag:"idle-timeout" help:"set the maximum idle body-read duration"`
	UnsetIdleTimeout bool         `flag:"unset-idle-timeout" help:"restore the default idle body-read duration"`

	// Validate runs immediately before Run on the same instance, so it parses
	// each repeated option once and apply consumes the result.
	edits assetEdits
}

// assetEdits is one update's parsed set algebra over variables and headers.
type assetEdits struct {
	variables      map[string]string
	unsetVariables map[string]bool
	headers        map[string]string
	removedHeaders map[string]string
}

func newUpdateCommand(runtime *runtime) *updateCommand {
	return &updateCommand{
		runtime:     runtime,
		URL:         newSingleValue("--url"),
		File:        newSingleValue("--file"),
		MaxSize:     newSingleValue("--max-size"),
		IdleTimeout: newSingleValue("--idle-timeout"),
	}
}

func (*updateCommand) Description() string { return "Update a desired remote artifact" }

func (command *updateCommand) Validate() error {
	if err := command.runtime.Output.ValidateOptions(); err != nil {
		return err
	}
	if command.Pin.set && command.Unpin {
		return ErrPinUnpinConflict
	}
	if command.MaxSize.set && command.UnsetMaxSize || command.IdleTimeout.set && command.UnsetIdleTimeout {
		return ErrLimitResetConflict
	}
	sets, err := parseSets(command.Set)
	if err != nil {
		return err
	}
	unsets, err := parseVariableNames(command.Unset)
	if err != nil {
		return err
	}
	for key := range sets {
		if unsets[key] {
			return fmt.Errorf("%w: variable %q", ErrEditConflict, key)
		}
	}
	headers, err := parseHeaders(command.Header)
	if err != nil {
		return err
	}
	removedHeaders, err := parseHeaderNames(command.RemoveHeader)
	if err != nil {
		return err
	}
	for key := range headers {
		if _, exists := removedHeaders[strings.ToLower(key)]; exists {
			return fmt.Errorf("%w: header %q", ErrEditConflict, key)
		}
	}
	command.edits = assetEdits{variables: sets, unsetVariables: unsets, headers: headers, removedHeaders: removedHeaders}
	if command.MaxSize.set {
		if _, err := asset.ParseMaxSize(command.MaxSize.value); err != nil {
			return err
		}
	}
	if command.IdleTimeout.set {
		if _, err := asset.ParseIdleTimeout(command.IdleTimeout.value); err != nil {
			return err
		}
	}
	if !command.hasRequestedEdit() {
		return ErrNoUpdate
	}
	return nil
}

func (command *updateCommand) hasRequestedEdit() bool {
	return command.URL.set || command.File.set || len(command.Set) > 0 || len(command.Unset) > 0 ||
		len(command.Header) > 0 || len(command.RemoveHeader) > 0 || command.Pin.set || command.Unpin ||
		command.MaxSize.set || command.UnsetMaxSize || command.IdleTimeout.set || command.UnsetIdleTimeout
}

func (command *updateCommand) Run(ctx context.Context) error {
	paths, err := command.runtime.project()
	if err != nil {
		return err
	}
	return paths.WithLock(ctx, func(ctx context.Context) error {
		value, err := manifest.Load(paths.Manifest())
		if err != nil {
			return err
		}
		current, exists := value.Files[command.Name]
		if !exists {
			return fault.NewConfigurationError(ErrAssetNotFound, fault.WithAsset(command.Name))
		}
		candidate := cloneAsset(current)
		if err := command.apply(&candidate); err != nil {
			return fault.NewConfigurationError(err, fault.WithAsset(command.Name))
		}
		value.Files[command.Name] = candidate
		resolved, err := manifest.Resolve(value)
		if err != nil {
			return err
		}
		resolvedAsset, ok := resolved.Get(command.Name)
		if !ok {
			return fault.NewConfigurationError(ErrAssetNotFound, fault.WithAsset(command.Name))
		}
		if command.Pin.calculate {
			candidate.Pin, err = command.runtime.calculatePin(ctx, paths, resolvedAsset)
			if err != nil {
				return err
			}
			value.Files[command.Name] = candidate
		}
		current = cloneAsset(current)
		normalizeAssetMaps(&current)
		normalizeAssetMaps(&candidate)
		if reflect.DeepEqual(current, candidate) {
			return fault.NewConfigurationError(ErrNoUpdate, fault.WithAsset(command.Name))
		}
		value.Files[command.Name] = candidate
		if err := manifest.Write(paths.Manifest(), value); err != nil {
			return err
		}
		result := output.Result{Name: command.Name, Status: "updated", File: resolvedAsset.ResolvedFile, Digest: candidate.Pin}
		return command.runtime.Output.Success("update", paths, []output.Result{result}, nil)
	})
}

func (command *updateCommand) apply(file *manifest.Asset) error {
	if command.URL.set {
		file.URL = command.URL.value
	}
	if command.File.set {
		file.File = command.File.value
	}
	sets := command.edits.variables
	if len(sets) > 0 && file.Variables == nil {
		file.Variables = make(map[string]string)
	}
	for key, value := range sets {
		file.Variables[key] = value
	}
	for key := range command.edits.unsetVariables {
		if _, exists := file.Variables[key]; !exists {
			return fmt.Errorf("%w %q", ErrUnknownUnset, key)
		}
		delete(file.Variables, key)
	}
	headers := command.edits.headers
	if len(headers) > 0 && file.Headers == nil {
		file.Headers = make(map[string]string)
	}
	for name, value := range headers {
		if existing, exists := headerKey(file.Headers, name); exists {
			delete(file.Headers, existing)
		}
		file.Headers[name] = value
	}
	for normalized, requested := range command.edits.removedHeaders {
		existing, exists := headerKey(file.Headers, normalized)
		if !exists {
			return fmt.Errorf("%w %q", ErrUnknownHeaderRemoval, requested)
		}
		delete(file.Headers, existing)
	}
	if command.Unpin {
		file.Pin = ""
	}
	if command.Pin.digest != "" {
		file.Pin = command.Pin.digest
	}
	if command.MaxSize.set {
		file.MaxSize = command.MaxSize.value
	} else if command.UnsetMaxSize {
		file.MaxSize = ""
	}
	if command.IdleTimeout.set {
		file.IdleTimeout = command.IdleTimeout.value
	} else if command.UnsetIdleTimeout {
		file.IdleTimeout = ""
	}
	normalizeAssetMaps(file)
	return nil
}

func cloneAsset(value manifest.Asset) manifest.Asset {
	value.Variables = maps.Clone(value.Variables)
	value.Headers = maps.Clone(value.Headers)
	return value
}

// normalizeAssetMaps gives semantically empty maps one representation so
// update can reject commands that do not change persisted configuration.
func normalizeAssetMaps(value *manifest.Asset) {
	if len(value.Variables) == 0 {
		value.Variables = nil
	}
	if len(value.Headers) == 0 {
		value.Headers = nil
	}
}

// headerKey finds the persisted spelling of a case-insensitive HTTP header.
func headerKey(headers map[string]string, requested string) (string, bool) {
	for name := range headers {
		if strings.EqualFold(name, requested) {
			return name, true
		}
	}
	return "", false
}
