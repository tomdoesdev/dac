package output

import "errors"

// ErrConflictingModes marks output modes whose guarantees cannot both apply.
var ErrConflictingModes = errors.New("--json and --quiet cannot be combined")
