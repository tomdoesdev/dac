// Package sys syncs a directory.
//
// This package holds every build constraint in the atomic module. The commit
// logic and the public API build on all platforms, so a platform that cannot
// sync a directory cannot make the API and the stub disagree.
package sys
