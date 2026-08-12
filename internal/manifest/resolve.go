package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"text/template"

	"github.com/tomdoesdev/dac/internal/project"
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
}

// Resolve renders and validates every entry together, including destination
// uniqueness. Callers use the sorted result before network or state changes.
func Resolve(value Manifest) ([]ResolvedAsset, error) {
	if err := Validate(value); err != nil {
		return nil, err
	}
	names := sortedKeys(value.Files)
	result := make([]ResolvedAsset, 0, len(names))
	seen := make(map[string]string, len(names))
	for _, name := range names {
		file := value.Files[name]
		resolvedURL, err := renderTemplate("url", file.URL, file.Variables)
		if err != nil {
			return nil, &project.Error{Kind: "configuration", Asset: name, Err: err}
		}
		if err := ValidateResolvedURL(resolvedURL); err != nil {
			return nil, &project.Error{Kind: "configuration", Asset: name, Err: err}
		}
		resolvedFile, err := renderTemplate("file", file.File, file.Variables)
		if err != nil {
			return nil, &project.Error{Kind: "configuration", Asset: name, Err: err}
		}
		if filename.Clean(resolvedFile) != resolvedFile {
			return nil, &project.Error{Kind: "configuration", Asset: name, Err: errors.New("rendered file must be one safe filename")}
		}
		key := filenameCollisionKey(resolvedFile)
		if existing, exists := seen[key]; exists {
			return nil, &project.Error{Kind: "configuration", Asset: name, Err: fmt.Errorf("file %q conflicts with asset %q", resolvedFile, existing)}
		}
		seen[key] = name
		result = append(result, ResolvedAsset{Name: name, Asset: file, ResolvedURL: resolvedURL, ResolvedFile: resolvedFile})
	}
	return result, nil
}

func parseTemplate(kind, value string) (*template.Template, error) {
	return template.New(kind).Option("missingkey=error").Parse(value)
}

func renderTemplate(kind, value string, variables map[string]string) (string, error) {
	templateValue, err := parseTemplate(kind, value)
	if err != nil {
		return "", err
	}
	var result bytes.Buffer
	if err := templateValue.Execute(&result, variables); err != nil {
		return "", fmt.Errorf("render %s template: %w", kind, err)
	}
	return result.String(), nil
}

// ValidateResolvedURL rejects URLs that cannot be safely requested or compared
// with persisted lock state.
func ValidateResolvedURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("rendered URL must be an absolute HTTP(S) URL without userinfo or fragment")
	}
	return nil
}

// InferFile returns the final URL path segment only when it is safe to manage.
func InferFile(rawURL string) (string, error) {
	result := filename.FromURL(rawURL)
	if result == "" {
		return "", errors.New("cannot infer a safe filename from URL; pass --file")
	}
	return result, nil
}

// filenameCollisionKey conservatively matches canonical and caseless aliases
// that supported filesystems may resolve to the same directory entry.
func filenameCollisionKey(value string) string {
	normalized := norm.NFC.String(value)
	return norm.NFC.String(cases.Fold().String(normalized))
}
