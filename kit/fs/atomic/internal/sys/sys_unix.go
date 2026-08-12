//go:build unix

package sys

import "os"

// SyncDir flushes a directory so that a rename in it survives a crash.
//
// A rename is atomic on its own, but the new name lives in the directory
// rather than in the file. Syncing the file alone leaves the contents on the
// disk under a name that the disk has never heard of.
//
// The directory opens for reading, because a directory does not open for
// writing. That is enough for fsync(2).
func SyncDir(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

// SyncRootDir flushes a directory addressed through a traversal-safe root.
func SyncRootDir(root *os.Root, path string) error {
	directory, err := root.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}
