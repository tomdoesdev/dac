package manifest

import (
	"bytes"
	"fmt"
	"net/url"
	"text/template"

	"github.com/tomdoesdev/dac/internal/asset"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/kit/fs/util/filename"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// ResolvedAsset is the safe, concrete configuration used by an operation.
type ResolvedAsset struct {
	Name string
	Asset
	ResolvedURL  string
	ResolvedFile string
	Transfer     asset.TransferPolicy
}

// Resolution is every resolved manifest asset in a stable order, with lookup
// by name. Callers previously rebuilt their own name index over a slice; the
// index is built once here so every workflow agrees on what the manifest
// declares.
type Resolution struct {
	assets []ResolvedAsset
	index  map[string]int
}

// All returns the resolved assets in sorted name order.
func (resolution Resolution) All() []ResolvedAsset { return resolution.assets }

// Len reports how many assets the manifest declares.
func (resolution Resolution) Len() int { return len(resolution.assets) }

// Get returns one resolved asset. The boolean distinguishes an absent name
// from an asset that happens to hold zero values.
func (resolution Resolution) Get(name string) (ResolvedAsset, bool) {
	index, exists := resolution.index[name]
	if !exists {
		return ResolvedAsset{}, false
	}
	return resolution.assets[index], true
}

// Has reports whether the manifest declares name.
func (resolution Resolution) Has(name string) bool {
	_, exists := resolution.index[name]
	return exists
}

// Resolve renders and validates every entry together, including destination
// uniqueness. Callers use the sorted result before network or state changes.
func Resolve(value Manifest) (Resolution, error) {
	if err := Validate(value); err != nil {
		return Resolution{}, err
	}
	names := sortedKeys(value.Files)
	result := make([]ResolvedAsset, 0, len(names))
	seen := make(map[string]string, len(names))
	for _, name := range names {
		file := value.Files[name]
		data := templateData(value.Globals, file.Variables)
		resolvedURL, err := renderTemplate("url", file.URL, data)
		if err != nil {
			return Resolution{}, fault.NewConfigurationError(err, fault.WithAsset(name))
		}
		if err := ValidateResolvedURL(resolvedURL); err != nil {
			return Resolution{}, fault.NewConfigurationError(err, fault.WithAsset(name))
		}
		resolvedFile, err := renderTemplate("file", file.File, data)
		if err != nil {
			return Resolution{}, fault.NewConfigurationError(err, fault.WithAsset(name))
		}
		if filename.Clean(resolvedFile) != resolvedFile {
			return Resolution{}, fault.NewConfigurationError(ErrUnsafeResolvedFile, fault.WithAsset(name))
		}
		key := filenameCollisionKey(resolvedFile)
		if existing, exists := seen[key]; exists {
			return Resolution{}, fault.NewConfigurationError(fmt.Errorf("%w: file %q conflicts with asset %q", ErrResolvedFileConflict, resolvedFile, existing), fault.WithAsset(name))
		}
		seen[key] = name
		transfer, err := parseTransfer(file)
		if err != nil {
			return Resolution{}, fault.NewConfigurationError(err, fault.WithAsset(name))
		}
		result = append(result, ResolvedAsset{Name: name, Asset: file, ResolvedURL: resolvedURL, ResolvedFile: resolvedFile, Transfer: transfer})
	}
	index := make(map[string]int, len(result))
	for position, item := range result {
		index[item.Name] = position
	}
	return Resolution{assets: result, index: index}, nil
}

func parseTemplate(kind, value string) (*template.Template, error) {
	return template.New(kind).Option("missingkey=error").Parse(value)
}

// GlobalNamespace is the template field that addresses project-wide variables,
// as in {{ .global.VERSION }}. It is deliberately lower case: the variable
// grammar requires an upper-case initial character, so no asset variable can
// ever occupy the namespace or be mistaken for one when reading a template.
const GlobalNamespace = "global"

// templateData exposes an asset's own variables at the top level, for the
// {{ .VERSION }} form assets already use, and the manifest's globals only
// under their namespace. Globals are never merged into the top level: a
// template must name where a value came from, so adding a global cannot
// silently change what an existing asset resolves to.
func templateData(globals, variables map[string]string) map[string]any {
	data := make(map[string]any, len(variables)+1)
	for key, value := range variables {
		data[key] = value
	}
	// The namespace is always present, even when the manifest declares no
	// globals, so an undefined reference reports the key the template asked
	// for rather than a missing namespace.
	data[GlobalNamespace] = globals
	return data
}

func renderTemplate(kind, value string, data map[string]any) (string, error) {
	templateValue, err := parseTemplate(kind, value)
	if err != nil {
		return "", err
	}
	var result bytes.Buffer
	if err := templateValue.Execute(&result, data); err != nil {
		return "", fmt.Errorf("%w %q: %w", ErrRenderTemplate, kind, err)
	}
	return result.String(), nil
}

// ValidateResolvedURL rejects URLs that cannot be safely requested or compared
// with persisted lock state.
func ValidateResolvedURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Fragment != "" {
		return ErrInvalidResolvedURL
	}
	return nil
}

// InferFile returns the final URL path segment only when it is safe to manage.
func InferFile(rawURL string) (string, error) {
	result := filename.FromURL(rawURL)
	if result == "" {
		return "", ErrCannotInferFile
	}
	return result, nil
}

// filenameCollisionKey conservatively matches canonical and caseless aliases
// that supported filesystems may resolve to the same directory entry.
func filenameCollisionKey(value string) string {
	normalized := norm.NFC.String(value)
	return norm.NFC.String(cases.Fold().String(normalized))
}
