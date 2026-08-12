package fault

import "fmt"

// ErrorKind is the stable category exposed in structured output and used to
// choose process exit codes.
type ErrorKind string

const (
	ErrorKindConfiguration ErrorKind = "configuration"
	ErrorKindFilesystem    ErrorKind = "filesystem"
	ErrorKindIntegrity     ErrorKind = "integrity"
	ErrorKindNetwork       ErrorKind = "network"
	ErrorKindCancelled     ErrorKind = "cancelled"
	ErrorKindUnsupported   ErrorKind = "unsupported"
)

// Error describes an operation failure without requiring callers to parse
// human-oriented text. Its fields are private so every value has a known kind
// and an underlying cause.
type Error struct {
	kind     ErrorKind
	asset    string
	recovery Recovery
	expected string
	received string
	err      error
}

// Recovery names the dac command that resolves a failure. Packages describe
// the remedy structurally rather than as text, because turning it into a
// pasteable command line requires shell quoting that only the process
// boundary should decide.
type Recovery struct {
	// Command is the dac subcommand to run, such as "init" or "lock".
	Command string
	// Flags are literal option words, such as "--all".
	Flags []string
	// Assets are opaque asset names. They may contain shell metacharacters or
	// begin with a dash, so a renderer must quote them and place them after
	// the option terminator.
	Assets []string
}

// Empty reports a recovery that names no command to run.
func (recovery Recovery) Empty() bool { return recovery.Command == "" }

func (err *Error) Error() string {
	if err.asset != "" {
		return fmt.Sprintf("%s for asset %q: %v", err.kind, err.asset, err.err)
	}
	return fmt.Sprintf("%s: %v", err.kind, err.err)
}

func (err *Error) Unwrap() error { return err.err }

// Kind returns the stable operation category used by process-boundary output.
func (err *Error) Kind() ErrorKind { return err.kind }

// Message returns the underlying human-oriented failure without category or
// asset decoration, as required by structured output.
func (err *Error) Message() string { return err.err.Error() }

// Asset returns the logical asset associated with the failure, when present.
func (err *Error) Asset() string { return err.asset }

// Recovery returns the command that resolves the failure, when one applies.
func (err *Error) Recovery() Recovery { return err.recovery }

// Expected returns the expected integrity value, when present.
func (err *Error) Expected() string { return err.expected }

// Received returns the received integrity value, when present.
func (err *Error) Received() string { return err.received }

// ErrorOption adds structured context without exposing Error's representation
// or requiring a separate constructor for every combination of metadata.
type ErrorOption func(*Error)

// WithAsset associates an error with one logical asset.
func WithAsset(asset string) ErrorOption {
	return func(err *Error) { err.asset = asset }
}

// WithRecovery attaches the command that resolves the failure.
func WithRecovery(recovery Recovery) ErrorOption {
	return func(err *Error) { err.recovery = recovery }
}

// WithIntegrity records the values needed to diagnose an integrity failure.
func WithIntegrity(expected, received string) ErrorOption {
	return func(err *Error) {
		err.expected = expected
		err.received = received
	}
}

// NewConfigurationError reports invalid or inconsistent project input.
func NewConfigurationError(err error, options ...ErrorOption) error {
	return newError(ErrorKindConfiguration, err, options...)
}

// NewFilesystemError reports a failure while accessing local project state.
func NewFilesystemError(err error, options ...ErrorOption) error {
	return newError(ErrorKindFilesystem, err, options...)
}

// NewIntegrityError reports bytes that cannot satisfy the accepted state.
func NewIntegrityError(err error, options ...ErrorOption) error {
	return newError(ErrorKindIntegrity, err, options...)
}

// NewNetworkError reports a failure while communicating with a remote host.
func NewNetworkError(err error, options ...ErrorOption) error {
	return newError(ErrorKindNetwork, err, options...)
}

// NewCancelledError reports an operation stopped by context cancellation.
func NewCancelledError(err error, options ...ErrorOption) error {
	return newError(ErrorKindCancelled, err, options...)
}

// NewUnsupportedError reports a platform capability required by dac.
func NewUnsupportedError(err error, options ...ErrorOption) error {
	return newError(ErrorKindUnsupported, err, options...)
}

// newError centralizes nil preservation so wrappers remain safe in cleanup and
// error-joining paths where an optional operation may have succeeded.
func newError(kind ErrorKind, err error, options ...ErrorOption) error {
	if err == nil {
		return nil
	}
	result := &Error{kind: kind, err: err}
	for _, option := range options {
		option(result)
	}
	return result
}
