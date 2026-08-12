package command

import (
	"context"

	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/output"
)

type addCommand struct {
	runtime     *runtime
	Name        string       `arg:"name"`
	URL         string       `arg:"url"`
	File        *singleValue `flag:"file" help:"local filename template"`
	Set         []string     `flag:"set" help:"set an artifact variable (KEY=VALUE)"`
	Header      []string     `flag:"header" help:"set an HTTP header (NAME=VALUE)"`
	Pin         pinValue     `flag:"pin" help:"calculate or require a SHA-256 pin"`
	MaxSize     *singleValue `flag:"max-size" help:"maximum response body size"`
	IdleTimeout *singleValue `flag:"idle-timeout" help:"maximum idle body-read duration"`

	// Validate runs immediately before Run on the same instance, so it parses
	// each repeated option once and Run consumes the result.
	variables map[string]string
	headers   map[string]string
}

func newAddCommand(runtime *runtime) *addCommand {
	return &addCommand{
		runtime:     runtime,
		File:        newSingleValue("--file"),
		MaxSize:     newSingleValue("--max-size"),
		IdleTimeout: newSingleValue("--idle-timeout"),
	}
}

func (*addCommand) Description() string { return "Add a desired remote artifact" }
func (command *addCommand) Validate() error {
	if err := command.runtime.Output.ValidateOptions(); err != nil {
		return err
	}
	variables, err := parseSets(command.Set)
	if err != nil {
		return err
	}
	headers, err := parseHeaders(command.Header)
	if err != nil {
		return err
	}
	command.variables, command.headers = variables, headers
	if command.MaxSize.set {
		if _, err := manifest.ParseMaxSize(command.MaxSize.value); err != nil {
			return err
		}
	}
	if command.IdleTimeout.set {
		if _, err := manifest.ParseIdleTimeout(command.IdleTimeout.value); err != nil {
			return err
		}
	}
	return nil
}

func (command *addCommand) Run(ctx context.Context) error {
	paths, err := command.runtime.project()
	if err != nil {
		return err
	}
	return paths.WithLock(ctx, func(ctx context.Context) error {
		value, err := manifest.Load(paths.Manifest())
		if err != nil {
			return err
		}
		if _, exists := value.Files[command.Name]; exists {
			return fault.NewConfigurationError(ErrAssetAlreadyExists, fault.WithAsset(command.Name))
		}
		fileName := command.File.value
		if !command.File.set {
			fileName, err = manifest.InferFile(command.URL)
			if err != nil {
				return fault.NewConfigurationError(err, fault.WithAsset(command.Name))
			}
		}
		candidate := manifest.Asset{URL: command.URL, File: fileName, Variables: command.variables, Headers: command.headers}
		if command.MaxSize.set {
			candidate.MaxSize = command.MaxSize.value
		}
		if command.IdleTimeout.set {
			candidate.IdleTimeout = command.IdleTimeout.value
		}
		if command.Pin.digest != "" {
			candidate.Pin = command.Pin.digest
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
		if err := manifest.Write(paths.Manifest(), value); err != nil {
			return err
		}
		result := output.Result{Name: command.Name, Status: "added", File: resolvedAsset.ResolvedFile, Digest: candidate.Pin}
		return command.runtime.Output.Success("add", paths, []output.Result{result}, nil)
	})
}
