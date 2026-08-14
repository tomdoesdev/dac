package command

import (
	"fmt"
	"strings"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/manifest"
	"github.com/tomdoesdev/dac/internal/output"
	"github.com/tomdoesdev/dac/internal/project"
	"github.com/tomdoesdev/kit/cli"
)

// Dependencies are the process-scoped concrete services shared by commands.
type Dependencies struct {
	Output     *output.Writer
	Downloader *asset.Downloader
	CWD        func() (string, error)
}

type runtime struct{ Dependencies }

// Register installs dac's complete command surface on app.
func Register(app *cli.App, dependencies Dependencies) {
	runtime := &runtime{Dependencies: dependencies}
	app.MustAddCommand("init", &initCommand{runtime: runtime})
	app.MustAddCommand("add <urls...>", newAddCommand(runtime))
	app.MustAddCommand("update [name]", newUpdateCommand(runtime))
	app.MustAddCommand("pull [names...]", &pullCommand{runtime: runtime})
}

func (runtime *runtime) project() (project.Paths, error) {
	directory, err := runtime.CWD()
	if err != nil {
		return project.Paths{}, fault.NewFilesystemError(err)
	}
	return project.Discover(directory)
}

func assetRequest(resolved manifest.ResolvedAsset) asset.Request {
	return asset.Request{Name: resolved.Name, URL: resolved.ResolvedURL, File: resolved.ResolvedFile, Headers: resolved.Headers, Policy: resolved.Transfer}
}

// assignments parses repeated NAME=VALUE options without allowing a later
// spelling to silently replace an earlier value in the same invocation.
func assignments(values []string, malformed, duplicate error) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for _, value := range values {
		name, item, found := strings.Cut(value, "=")
		if !found || name == "" {
			return nil, malformed
		}
		if _, exists := result[name]; exists {
			return nil, fmt.Errorf("%w %q", duplicate, name)
		}
		result[name] = item
	}
	return result, nil
}

// scopedVariableAssignments keeps one flag's asset and project destinations
// separate after parsing so commands never reinterpret the $. prefix at runtime.
type scopedVariableAssignments struct {
	asset   map[string]string
	globals map[string]string
}

// parseSets validates assignments and uses the explicit $. prefix to route
// project globals without giving --set a second, parallel flag grammar.
func parseSets(values []string) (scopedVariableAssignments, error) {
	return parseScopedVariableAssignments(values, ErrInvalidSet, ErrDuplicateSet)
}

// parseScopedVariableAssignments validates names after removing only the
// documented $. prefix; every other spelling remains an asset-local name.
func parseScopedVariableAssignments(values []string, malformed, duplicate error) (scopedVariableAssignments, error) {
	result, err := assignments(values, malformed, duplicate)
	if err != nil {
		return scopedVariableAssignments{}, err
	}
	parsed := scopedVariableAssignments{asset: make(map[string]string), globals: make(map[string]string)}
	for name, value := range result {
		if strings.HasPrefix(name, "$.") {
			key := strings.TrimPrefix(name, "$.")
			if !manifest.ValidVariableName(key) {
				return scopedVariableAssignments{}, fmt.Errorf("%w %q", manifest.ErrInvalidGlobalName, key)
			}
			parsed.globals[key] = value
			continue
		}
		if !manifest.ValidVariableName(name) {
			return scopedVariableAssignments{}, fmt.Errorf("%w %q", manifest.ErrInvalidVariableName, name)
		}
		parsed.asset[name] = value
	}
	return parsed, nil
}
