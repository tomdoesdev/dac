package manifest

import "errors"

var (
	// ErrDecode marks TOML that cannot be decoded as a dac manifest.
	ErrDecode = errors.New("decode dac.toml")
	// ErrAlreadyExists marks an atomic manifest create that would replace a file.
	ErrAlreadyExists = errors.New("dac.toml already exists")
	// ErrUnsupportedVersion marks a manifest version dac cannot interpret.
	ErrUnsupportedVersion = errors.New("unsupported dac.toml version")
	// ErrInvalidAssetName marks an unsafe logical asset identifier.
	ErrInvalidAssetName = errors.New("invalid asset name")
	// ErrInvalidAssetEncoding marks non-UTF-8 manifest asset fields.
	ErrInvalidAssetEncoding = errors.New("url, file, pin, and transfer settings must be valid UTF-8")
	// ErrMissingAssetLocation marks an asset without both required templates.
	ErrMissingAssetLocation = errors.New("url and file are required")
	// ErrInvalidPin marks a pin outside the supported digest format.
	ErrInvalidPin = errors.New("invalid pin")
	// ErrInvalidVariableName marks a variable key outside dac's identifier grammar.
	ErrInvalidVariableName = errors.New("invalid variable name")
	// ErrInvalidVariableValue marks a variable value that is not valid UTF-8.
	ErrInvalidVariableValue = errors.New("invalid variable value")
	// ErrUnsafeResolvedFile marks a rendered destination that is not one filename.
	ErrUnsafeResolvedFile = errors.New("rendered file must be one safe filename")
	// ErrResolvedFileConflict marks two assets that resolve to the same destination.
	ErrResolvedFileConflict = errors.New("resolved file conflicts with another asset")
	// ErrRenderTemplate marks a failure while executing a manifest template.
	ErrRenderTemplate = errors.New("failed to render template")
	// ErrInvalidResolvedURL marks a rendered URL unsafe for requests or persistence.
	ErrInvalidResolvedURL = errors.New("rendered URL must be an absolute HTTP(S) URL without userinfo or fragment")
	// ErrCannotInferFile marks a URL without a safe final path segment.
	ErrCannotInferFile = errors.New("cannot infer a safe filename from URL; pass --file")
)
