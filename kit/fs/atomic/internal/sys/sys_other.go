//go:build !unix

package sys

// SyncDir does nothing where a directory cannot be synced.
//
// This is a stub so that the package builds everywhere. A commit on such a
// platform still renames, and the rename is still atomic; only its durability
// after a crash is weaker.
func SyncDir(path string) error {
	return nil
}
