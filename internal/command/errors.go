package command

import "errors"

var (
	// ErrAssetAlreadyExists marks an add that would replace a manifest asset.
	ErrAssetAlreadyExists = errors.New("asset already exists")
	// ErrManifestAlreadyExists marks initialization of an existing project.
	ErrManifestAlreadyExists = errors.New("dac.toml already exists")
	// ErrTargetedLockNeedsExisting marks an incomplete initial targeted lock.
	ErrTargetedLockNeedsExisting = errors.New("targeted lock requires a complete existing dac.lock")
	// ErrAssetNotFound marks a command selection absent from the manifest.
	ErrAssetNotFound = errors.New("asset does not exist")
	// ErrAssetSelectedMultipleTimes marks a duplicated targeted lock selection.
	ErrAssetSelectedMultipleTimes = errors.New("asset selected more than once")
	// ErrPinRepeated marks more than one value supplied for --pin.
	ErrPinRepeated = errors.New("--pin may be supplied only once")
	// ErrOfflineForceConflict marks mutually exclusive pull modes.
	ErrOfflineForceConflict = errors.New("--offline and --force cannot be combined")
	// ErrOfflineVerification marks local files that failed offline verification.
	ErrOfflineVerification = errors.New("offline verification failed")
	// ErrInvalidSet marks a --set value without the required key/value form.
	ErrInvalidSet = errors.New("--set must be KEY=VALUE")
	// ErrDuplicateSet marks a repeated key in --set options.
	ErrDuplicateSet = errors.New("--set repeats key")
	// ErrPinUnpinConflict marks mutually exclusive update modes.
	ErrPinUnpinConflict = errors.New("--pin and --unpin cannot be combined")
)
