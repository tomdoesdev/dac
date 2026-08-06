package atomic

import "io/fs"

// Option changes how this package writes a file.
type Option func(*settings)

// settings holds the values that the options change.
type settings struct {
	dirMode    fs.FileMode
	makeDirs   bool
	tempPrefix string
	sync       bool
}

// DefaultTempPrefix names the temporary files this package creates.
//
// A process that is killed outright leaves one behind. The name starts with a
// dot so that it stays out of the way, and it is a known prefix so that a
// caller that wants to collect the leftovers has something to match.
const DefaultTempPrefix = ".tmp-"

// defaultDirMode is the mode of a directory that this package creates.
const defaultDirMode = 0o755

func newSettings(options []Option) settings {
	current := settings{
		dirMode:    defaultDirMode,
		makeDirs:   true,
		tempPrefix: DefaultTempPrefix,
		sync:       true,
	}
	for _, option := range options {
		option(&current)
	}
	return current
}

// WithDirMode sets the mode of a directory that this package creates.
// The default is 0o755. The mode of a directory that exists already does not
// change. A umask applies, because this mode reaches mkdir(2) rather than
// chmod(2).
func WithDirMode(mode fs.FileMode) Option {
	return func(current *settings) { current.dirMode = mode }
}

// WithoutMkdir stops this package from making the directories it writes into.
// A directory that does not exist then fails with fs.ErrNotExist.
func WithoutMkdir() Option {
	return func(current *settings) { current.makeDirs = false }
}

// WithTempPrefix names the temporary file. The default is DefaultTempPrefix.
// An empty prefix keeps the default, because a temporary file with no prefix
// is hard to tell from a real one.
func WithTempPrefix(prefix string) Option {
	return func(current *settings) {
		if prefix != "" {
			current.tempPrefix = prefix
		}
	}
}

// WithoutSync stops the commit from flushing the file and its directory.
//
// The write is still atomic: a reader sees the old file or the new one. What
// it loses is durability, so a crash can leave the old file in place. Use it
// for content that a later run can make again, and where an fsync for each
// file is the whole cost of the operation.
func WithoutSync() Option {
	return func(current *settings) { current.sync = false }
}
