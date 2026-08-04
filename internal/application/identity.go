package application

// This file holds the rules that make a version mean something. DAC does not
// order versions, compare them, or know what one means; it only guarantees that
// the version written down is the version that comes back. Two rules are enough
// for that:
//
//   - A locked coordinate always names the same bytes. Nothing rewrites the
//     digest under a version that is already locked.
//   - Two versions of one asset never name the same bytes. A version invented
//     for bytes that already had one is a label, not a version.
//
// Both are local: they read the project's own two files and nothing else. What
// belongs to a package manager and stays out of here is the other direction —
// deciding which version to use, ordering them, or asking an origin what exists.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tomdoesdev/dac/internal/coord"
	"github.com/tomdoesdev/dac/internal/fault"
	"github.com/tomdoesdev/dac/internal/project"
)

// checkRebind refuses to change the bytes a locked coordinate names.
//
// This is what a version bought before it was part of the key and what it keeps
// now. An origin that replaces the bytes behind a stable URL, or a manifest
// edited to point one version somewhere else, both arrive here: the coordinate
// did not move, so the bytes may not either. The way forward is a new version,
// and --rebind is for the project that genuinely tracks a rolling source and
// has decided to accept it.
func checkRebind(name coord.Coordinate, locked, resolved project.LockAsset) error {
	if locked.Digest == resolved.Digest {
		return nil
	}
	return &fault.Error{
		Code:    "version_rebind",
		Message: "The origin no longer serves the bytes this version is locked to. Add a new version, or pass --rebind to change what this one means.",
		Details: map[string]any{
			"asset":          name.String(),
			"lockedDigest":   locked.Digest,
			"resolvedDigest": resolved.Digest,
		},
		Cause: fmt.Errorf("%s is locked to %s but resolved to %s", name, locked.Digest, resolved.Digest),
	}
}

// checkRetired reports a version that resolved to bytes a version this project
// used to have.
//
// It is the case the lock file alone cannot see. Editing a version in place
// takes the old coordinate out of the manifest, so the two never appear
// together and nothing compares them — which is exactly the shape of bumping a
// version without changing the source. The previous lock file still remembers,
// and remembering is all this needs.
func checkRetired(resolved map[coord.Coordinate]project.LockAsset, old project.Lock) error {
	for _, name := range coord.Sorted(resolved) {
		if _, kept := old.Assets[name]; kept {
			continue
		}
		for _, previous := range coord.InGroup(old.Assets, name.Group()) {
			if _, current := resolved[previous]; current {
				// Still in the project, so CheckVersions has already
				// compared the two and this would only say it again.
				continue
			}
			if old.Assets[previous].Digest != resolved[name].Digest {
				continue
			}
			return &fault.Error{
				Code:    "version_collision",
				Message: "This version names the bytes a version this project retired already had. Point it at a different source, restore the version that named them, or pass --rebind to accept the rename.",
				Details: map[string]any{
					"asset":    name.Group().String(),
					"versions": []string{previous.Version, name.Version},
					"digest":   resolved[name].Digest,
				},
				Cause: fmt.Errorf("%s and the retired %s both name %s", name, previous, resolved[name].Digest),
			}
		}
	}
	return nil
}

// asCollision turns the lock invariant into the command error for it, and
// reports whether the error was one. The rule lives in project so that no Lock
// value can ever hold a collision; the wording lives here so that the operator
// gets an instruction rather than "the lock file is invalid".
//
// It answers two things at once because its callers want different ones. A
// caller with its own wording for everything else needs to know whether this
// error was a collision; a caller that has none needs a fault either way. The
// bool keeps those apart, so that neither ends up reading a nil error as
// success -- which is what a single function returning nil for "not a
// collision" invited, and what versionFault below exists to make impossible.
func asCollision(err error) (*fault.Error, bool) {
	var collision *project.VersionCollisionError
	if !errors.As(err, &collision) {
		return nil, false
	}
	return &fault.Error{
		Code:    "version_collision",
		Message: "Two versions of one asset name the same bytes. One of them is a label for bytes that already had a version: remove it, or point it at the source it was meant to have.",
		Details: map[string]any{
			"asset":    collision.Group.String(),
			"versions": collision.Versions,
			"digest":   collision.Digest,
		},
		Cause: collision,
	}, true
}

// versionFault names a failed version check. It always returns an error,
// because its caller has already decided that the check failed.
func versionFault(err error) error {
	if collision, found := asCollision(err); found {
		return collision
	}
	return fault.Wrap("lock_invalid", "DAC could not check the resolved assets.", err)
}

// lockFault names a lock failure. A version collision keeps its own code and
// instruction; everything else is a lock file DAC could not use.
func lockFault(code, message string, err error) error {
	if collision, found := asCollision(err); found {
		return collision
	}
	return fault.Wrap(code, message, err)
}

// unknownCoordinate reports a coordinate the subject does not have.
//
// It lists the versions the subject does have for that asset. Now that a
// project can hold several, "this asset coordinate is not here" is the answer
// to two different mistakes — a name nobody added, and a version that is one
// character off — and only one of them is worth a second look at the manifest.
//
// Subject is the noun the refusal names, because the same lookup is made
// against two different things: the project the caller is standing in, and the
// index of a dacpack that no project has to be behind at all. Telling somebody
// unpacking an archive that the project does not have an asset would point them
// at a file this command never read.
func unknownCoordinate[V any](subject string, name coord.Coordinate, assets map[coord.Coordinate]V) error {
	versions := coord.Versions(coord.InGroup(assets, name.Group()))
	if len(versions) == 0 {
		return &fault.Error{
			Code:    "asset_unknown",
			Message: "The " + subject + " does not have this asset.",
			Details: map[string]any{"asset": name.String()},
		}
	}
	return &fault.Error{
		Code:    "asset_unknown",
		Message: "The " + subject + " does not have this version of the asset.",
		Details: map[string]any{"asset": name.String(), "versions": versions},
		Cause:   fmt.Errorf("%s has %s", name.Group(), strings.Join(versions, ", ")),
	}
}

// sharedSources returns the versions of one asset already pointing at a URL.
//
// Two versions of one asset served from one URL is not wrong the way a
// collision is — the bytes there today are one version's, and the other's are
// whatever the origin used to send — but it is fragile in a way worth saying
// out loud, because a cold cache can only ever restore one of them. An add
// reports it and continues: the project that pins both and keeps them cached
// has made a choice DAC has no business refusing.
func sharedSources(manifest project.Manifest, name coord.Coordinate, url string) []string {
	shared := make([]string, 0, 2)
	for _, other := range coord.InGroup(manifest.Assets, name.Group()) {
		if other != name && manifest.Assets[other].URL == url {
			shared = append(shared, other.Version)
		}
	}
	return shared
}
