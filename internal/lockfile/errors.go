package lockfile

import "errors"

var (
	// ErrNotFound marks a required lockfile that does not exist.
	ErrNotFound = errors.New("dac.lock does not exist")
	// ErrDecode marks JSON that cannot be decoded as a dac lockfile.
	ErrDecode = errors.New("decode dac.lock")
	// ErrUnsupportedVersion marks a lockfile version dac cannot interpret.
	ErrUnsupportedVersion = errors.New("unsupported dac.lock version")
	// ErrMissingFiles marks a lockfile without its required files object.
	ErrMissingFiles = errors.New("dac.lock must contain files")
	// ErrInvalidEntry marks unsafe or incomplete persisted asset state.
	ErrInvalidEntry = errors.New("invalid lock entry")
	// ErrInvalidResolvedURL marks a persisted URL outside the manifest URL policy.
	ErrInvalidResolvedURL = errors.New("invalid resolved_url")
	// ErrInvalidDigest marks a persisted digest outside dac's supported format.
	ErrInvalidDigest = errors.New("invalid digest")
	// ErrResolutionChanged marks a manifest resolution that differs from locked state.
	ErrResolutionChanged = errors.New("resolution changed")
	// ErrPinChanged marks a manifest trust pin that differs from locked bytes.
	ErrPinChanged = errors.New("pin changed")
	// ErrStale marks lock state that no longer describes the manifest.
	ErrStale = errors.New("dac.lock is stale")
)
