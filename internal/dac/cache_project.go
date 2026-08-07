package dac

import (
	"slices"

	"github.com/tomdoesdev/dac/internal/project"
)

// This file holds projections of an already-read project that cache operations share.

// lockedDigests returns the digest of every locked asset, in manifest order.
func lockedDigests(manifest project.Manifest, lock project.Lock) []string {
	digests := make([]string, 0, len(lock.Assets))
	for _, name := range manifest.Coordinates() {
		digests = append(digests, lock.Assets[name].Digest)
	}
	return digests
}

// objectOwners maps each locked digest to the coordinates that resolve to it.
// It takes the project its caller has already read, because re-reading it would parse, normalize and
// re-validate every asset a second time within one command that cannot see the file change.
func objectOwners(manifest project.Manifest, lock project.Lock) map[string][]string {
	owners := map[string][]string{}
	for _, name := range manifest.Coordinates() {
		locked, exists := lock.Assets[name]
		if !exists {
			continue
		}
		owners[locked.Digest] = append(owners[locked.Digest], name.String())
	}
	for _, names := range owners {
		slices.Sort(names)
	}
	return owners
}
