package command

import (
	"errors"
	"fmt"

	"github.com/tomdoesdev/dac/internal/asset"
)

var (
	// ErrAssetAlreadyExists marks an add that would replace a manifest asset.
	ErrAssetAlreadyExists = errors.New("asset already exists")
	// ErrAssetNotFound marks a command selection absent from the manifest.
	ErrAssetNotFound = errors.New("asset does not exist")
	// ErrAssetSelectedMultipleTimes marks a duplicated pull selection.
	ErrAssetSelectedMultipleTimes = errors.New("asset selected more than once")
	// ErrJobsRepeated marks more than one value supplied for --jobs.
	ErrJobsRepeated = errors.New("--jobs may be supplied only once")
	// ErrInvalidJobs marks a download concurrency outside what dac will run.
	ErrInvalidJobs = fmt.Errorf("--jobs must be between 1 and %d", asset.MaxDownloadJobs)
	// ErrOfflineForceConflict marks mutually exclusive pull modes.
	ErrOfflineForceConflict = errors.New("--offline and --force cannot be combined")
	// ErrOfflineUpdateConflict prevents a network-free pull from accepting new bytes.
	ErrOfflineUpdateConflict = errors.New("--offline and --update-lockfile cannot be combined")
	// ErrOfflineVerification marks local files that failed offline verification.
	ErrOfflineVerification = errors.New("offline verification failed")
	// ErrInvalidSet marks a --set value without the required key/value form.
	ErrInvalidSet = errors.New("--set must be KEY=VALUE")
	// ErrDuplicateSet marks a repeated key in --set options.
	ErrDuplicateSet = errors.New("--set repeats key")
	// ErrVariableNotFound prevents --set from creating an unreferenced typo.
	ErrVariableNotFound = errors.New("variable does not exist")
	// ErrProjectVariableScope keeps nameless updates explicitly project-scoped.
	ErrProjectVariableScope = errors.New("project update requires $.KEY assignments")
	// ErrAssetVariableScope keeps named updates local to the selected asset.
	ErrAssetVariableScope = errors.New("asset update requires unscoped KEY assignments")
	// ErrNoUpdate marks an update that would not change desired state.
	ErrNoUpdate = errors.New("update makes no changes")
)
