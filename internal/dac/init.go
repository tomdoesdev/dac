package dac

import (
	"errors"
	"os"

	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
)

// InitResult reports the files created by init.
type InitResult struct {
	ManifestPath string `json:"manifestPath"`
	LockPath     string `json:"lockPath"`
}

// Init creates matching empty project files.
func (service *Service) Init(force bool) (InitResult, error) {
	if !force {
		for _, path := range []string{service.ManifestPath, service.LockPath} {
			if _, err := os.Stat(path); err == nil {
				return InitResult{}, fault.New("manifest_exists", "The project files already exist. Use --force to replace them.")
			} else if !errors.Is(err, os.ErrNotExist) {
				return InitResult{}, fault.Wrap("project_write_failed", "DAC could not check a project path.", err)
			}
		}
	}
	manifest, lock := project.Empty()
	if err := project.WritePair(service.ManifestPath, service.LockPath, manifest, lock); err != nil {
		return InitResult{}, fault.Wrap("project_write_failed", "DAC could not write the project files.", err)
	}
	return InitResult{ManifestPath: service.ManifestPath, LockPath: service.LockPath}, nil
}
