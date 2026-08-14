package manifest

import (
	"fmt"
	"net/url"
	"strings"

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
	return resolveNames(value, sortedKeys(value.Files), false)
}

// ResolveSelected renders only the named assets. Scoped pulls use it so an
// unrelated asset that is still awaiting a variable cannot block ready work.
func ResolveSelected(value Manifest, names []string) (Resolution, error) {
	return resolveNames(value, names, false)
}

// ValidateAvailable validates every asset whose referenced variables have
// values, while deliberately allowing the remaining desired state to be
// completed by later update commands.
func ValidateAvailable(value Manifest) error {
	_, err := resolveNames(value, sortedKeys(value.Files), true)
	return err
}

// resolveNames is the shared resolver for complete, scoped, and incrementally
// configured manifests, keeping safety and collision checks identical.
func resolveNames(value Manifest, names []string, allowIncomplete bool) (Resolution, error) {
	if err := Validate(value); err != nil {
		return Resolution{}, err
	}
	result := make([]ResolvedAsset, 0, len(names))
	seen := make(map[string]string, len(names))
	for _, name := range names {
		file, exists := value.Files[name]
		if !exists {
			return Resolution{}, fault.NewConfigurationError(fmt.Errorf("asset does not exist"), fault.WithAsset(name))
		}
		urlTemplate, _ := compileTemplate("url", file.URL)
		fileTemplate, _ := compileTemplate("file", file.File)
		urlReady := urlTemplate.canRender(value.Globals, file.Variables)
		fileReady := fileTemplate.canRender(value.Globals, file.Variables)
		resolvedURL, resolvedFile := "", ""
		if urlReady {
			var err error
			resolvedURL, err = urlTemplate.render(value.Globals, file.Variables)
			if err != nil {
				return Resolution{}, fault.NewConfigurationError(fmt.Errorf("%w: %w", ErrRenderTemplate, err), fault.WithAsset(name))
			}
			if err := ValidateResolvedURL(resolvedURL); err != nil {
				return Resolution{}, fault.NewConfigurationError(err, fault.WithAsset(name))
			}
		} else if !allowIncomplete {
			_, err := urlTemplate.render(value.Globals, file.Variables)
			return Resolution{}, fault.NewConfigurationError(fmt.Errorf("%w: %w", ErrRenderTemplate, err), fault.WithAsset(name))
		}
		if fileReady {
			var err error
			resolvedFile, err = fileTemplate.render(value.Globals, file.Variables)
			if err != nil {
				return Resolution{}, fault.NewConfigurationError(fmt.Errorf("%w: %w", ErrRenderTemplate, err), fault.WithAsset(name))
			}
			if filename.Clean(resolvedFile) != resolvedFile {
				return Resolution{}, fault.NewConfigurationError(ErrUnsafeResolvedFile, fault.WithAsset(name))
			}
			key := filenameCollisionKey(resolvedFile)
			if existing, exists := seen[key]; exists {
				return Resolution{}, fault.NewConfigurationError(fmt.Errorf("%w: file %q conflicts with asset %q", ErrResolvedFileConflict, resolvedFile, existing), fault.WithAsset(name))
			}
			seen[key] = name
		} else if !allowIncomplete {
			_, err := fileTemplate.render(value.Globals, file.Variables)
			return Resolution{}, fault.NewConfigurationError(fmt.Errorf("%w: %w", ErrRenderTemplate, err), fault.WithAsset(name))
		}
		if !urlReady || !fileReady {
			continue
		}
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
	path := rawURL
	if end := strings.IndexAny(path, "?#"); end >= 0 {
		path = path[:end]
	}
	segment := path[strings.LastIndex(path, "/")+1:]
	if strings.Contains(segment, "{{") || strings.Contains(segment, "}}") {
		return "", ErrCannotInferFile
	}
	templateValue, err := compileTemplate("url", rawURL)
	if err != nil {
		return "", err
	}
	provisional := templateValue.renderProvisional()
	if err := ValidateResolvedURL(provisional); err != nil {
		return "", err
	}
	result := filename.FromURL(provisional)
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
