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
	// ErrPinRepeated marks more than one value supplied for --pin.
	ErrPinRepeated = errors.New("--pin may be supplied only once")
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
	// ErrInvalidGset marks a --gset value without the required key/value form.
	ErrInvalidGset = errors.New("--gset must be KEY=VALUE")
	// ErrDuplicateGset marks a repeated key in --gset options.
	ErrDuplicateGset = errors.New("--gset repeats key")
	// ErrGlobalAlreadyExists marks a --gset that would rebind a global variable.
	ErrGlobalAlreadyExists = errors.New("global variable already exists")
	// ErrPinUnpinConflict marks mutually exclusive update modes.
	ErrPinUnpinConflict = errors.New("--pin and --unpin cannot be combined")
	// ErrOptionRepeated marks a singleton option supplied more than once.
	ErrOptionRepeated = errors.New("option may be supplied only once")
	// ErrHeaderFormat marks a header edit without NAME=VALUE form. It is named
	// apart from asset.ErrInvalidHeader, which classifies the HTTP grammar
	// rather than the command-line spelling.
	ErrHeaderFormat = errors.New("--header must be NAME=VALUE")
	// ErrDuplicateHeader marks equivalent repeated header names.
	ErrDuplicateHeader = errors.New("--header repeats name")
	// ErrUnknownUnset marks a variable requested for removal that is absent.
	ErrUnknownUnset = errors.New("--unset variable does not exist")
	// ErrUnknownHeaderRemoval marks a header requested for removal that is absent.
	ErrUnknownHeaderRemoval = errors.New("--remove-header name does not exist")
	// ErrEditConflict marks a key both set and removed in one update.
	ErrEditConflict = errors.New("the same value cannot be set and removed")
	// ErrNoUpdate marks an update that would not change desired state.
	ErrNoUpdate = errors.New("update makes no changes")
	// ErrLimitResetConflict marks a transfer limit both set and reset.
	ErrLimitResetConflict = errors.New("a transfer limit cannot be set and unset together")
)
